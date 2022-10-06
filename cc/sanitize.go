// Copyright 2016 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cc

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/blueprint"
	"github.com/google/blueprint/proptools"

	"android/soong/android"
	"android/soong/cc/config"
	"android/soong/snapshot"
)

var (
	// Any C flags added by sanitizer which libTooling tools may not
	// understand also need to be added to ClangLibToolingUnknownCflags in
	// cc/config/clang.go

	asanCflags = []string{
		"-fno-omit-frame-pointer",
	}
	asanLdflags = []string{"-Wl,-u,__asan_preinit"}

	hwasanCflags = []string{
		"-fno-omit-frame-pointer",
		"-Wno-frame-larger-than=",
		"-fsanitize-hwaddress-abi=platform",
		"-mllvm", "-hwasan-use-after-scope=1",
	}

	// ThinLTO performs codegen during link time, thus these flags need to
	// passed to both CFLAGS and LDFLAGS.
	hwasanCommonflags = []string{
		// The following improves debug location information
		// availability at the cost of its accuracy. It increases
		// the likelihood of a stack variable's frame offset
		// to be recorded in the debug info, which is important
		// for the quality of hwasan reports. The downside is a
		// higher number of "optimized out" stack variables.
		// b/112437883.
		"-instcombine-lower-dbg-declare=0",
		// TODO(b/159343917): HWASan and GlobalISel don't play nicely, and
		// GlobalISel is the default at -O0 on aarch64.
		"--aarch64-enable-global-isel-at-O=-1",
		"-fast-isel=false",
	}

	cfiCflags = []string{"-flto", "-fsanitize-cfi-cross-dso",
		"-fsanitize-blacklist=external/compiler-rt/lib/cfi/cfi_blocklist.txt"}
	// -flto and -fvisibility are required by clang when -fsanitize=cfi is
	// used, but have no effect on assembly files
	cfiAsflags = []string{"-flto", "-fvisibility=default"}
	cfiLdflags = []string{"-flto", "-fsanitize-cfi-cross-dso", "-fsanitize=cfi",
		"-Wl,-plugin-opt,O1"}
	cfiExportsMapPath = "build/soong/cc/config/cfi_exports.map"

	intOverflowCflags = []string{"-fsanitize-blacklist=build/soong/cc/config/integer_overflow_blocklist.txt"}

	minimalRuntimeFlags = []string{"-fsanitize-minimal-runtime", "-fno-sanitize-trap=integer,undefined",
		"-fno-sanitize-recover=integer,undefined"}
	hwasanGlobalOptions = []string{"heap_history_size=1023", "stack_history_size=512",
		"export_memory_stats=0", "max_malloc_fill_size=4096", "malloc_fill_byte=0"}
	memtagStackCommonFlags = []string{"-march=armv8-a+memtag"}
)

type SanitizerType int

const (
	Asan SanitizerType = iota + 1
	Hwasan
	tsan
	intOverflow
	scs
	Fuzzer
	Memtag_heap
	Memtag_stack
	cfi // cfi is last to prevent it running before incompatible mutators
)

var Sanitizers = []SanitizerType{
	Asan,
	Hwasan,
	tsan,
	intOverflow,
	scs,
	Fuzzer,
	Memtag_heap,
	Memtag_stack,
	cfi, // cfi is last to prevent it running before incompatible mutators
}

// Name of the sanitizer variation for this sanitizer type
func (t SanitizerType) variationName() string {
	switch t {
	case Asan:
		return "asan"
	case Hwasan:
		return "hwasan"
	case tsan:
		return "tsan"
	case intOverflow:
		return "intOverflow"
	case cfi:
		return "cfi"
	case scs:
		return "scs"
	case Memtag_heap:
		return "memtag_heap"
	case Memtag_stack:
		return "memtag_stack"
	case Fuzzer:
		return "fuzzer"
	default:
		panic(fmt.Errorf("unknown SanitizerType %d", t))
	}
}

// This is the sanitizer names in SANITIZE_[TARGET|HOST]
func (t SanitizerType) name() string {
	switch t {
	case Asan:
		return "address"
	case Hwasan:
		return "hwaddress"
	case Memtag_heap:
		return "memtag_heap"
	case Memtag_stack:
		return "memtag_stack"
	case tsan:
		return "thread"
	case intOverflow:
		return "integer_overflow"
	case cfi:
		return "cfi"
	case scs:
		return "shadow-call-stack"
	case Fuzzer:
		return "fuzzer"
	default:
		panic(fmt.Errorf("unknown SanitizerType %d", t))
	}
}

func (t SanitizerType) registerMutators(ctx android.RegisterMutatorsContext) {
	switch t {
	case cfi, Hwasan, Asan, tsan, Fuzzer, scs:
		sanitizer := &sanitizerSplitMutator{t}
		ctx.TopDown(t.variationName()+"_markapexes", sanitizer.markSanitizableApexesMutator)
		ctx.Transition(t.variationName(), sanitizer)
	case Memtag_heap, Memtag_stack, intOverflow:
		// do nothing
	default:
		panic(fmt.Errorf("unknown SanitizerType %d", t))
	}
}

func (*Module) SanitizerSupported(t SanitizerType) bool {
	switch t {
	case Asan:
		return true
	case Hwasan:
		return true
	case tsan:
		return true
	case intOverflow:
		return true
	case cfi:
		return true
	case scs:
		return true
	case Fuzzer:
		return true
	case Memtag_heap:
		return true
	case Memtag_stack:
		return true
	default:
		return false
	}
}

// incompatibleWithCfi returns true if a sanitizer is incompatible with CFI.
func (t SanitizerType) incompatibleWithCfi() bool {
	return t == Asan || t == Fuzzer || t == Hwasan
}

type SanitizeUserProps struct {
	// Prevent use of any sanitizers on this module
	Never *bool `android:"arch_variant"`

	// ASan (Address sanitizer), incompatible with static binaries.
	// Always runs in a diagnostic mode.
	// Use of address sanitizer disables cfi sanitizer.
	// Hwaddress sanitizer takes precedence over this sanitizer.
	Address *bool `android:"arch_variant"`
	// TSan (Thread sanitizer), incompatible with static binaries and 32 bit architectures.
	// Always runs in a diagnostic mode.
	// Use of thread sanitizer disables cfi and scudo sanitizers.
	// Hwaddress sanitizer takes precedence over this sanitizer.
	Thread *bool `android:"arch_variant"`
	// HWASan (Hardware Address sanitizer).
	// Use of hwasan sanitizer disables cfi, address, thread, and scudo sanitizers.
	Hwaddress *bool `android:"arch_variant"`

	// Undefined behavior sanitizer
	All_undefined *bool `android:"arch_variant"`
	// Subset of undefined behavior sanitizer
	Undefined *bool `android:"arch_variant"`
	// List of specific undefined behavior sanitizers to enable
	Misc_undefined []string `android:"arch_variant"`
	// Fuzzer, incompatible with static binaries.
	Fuzzer *bool `android:"arch_variant"`
	// safe-stack sanitizer, incompatible with 32-bit architectures.
	Safestack *bool `android:"arch_variant"`
	// cfi sanitizer, incompatible with asan, hwasan, fuzzer, or Darwin
	Cfi *bool `android:"arch_variant"`
	// signed/unsigned integer overflow sanitizer, incompatible with Darwin.
	Integer_overflow *bool `android:"arch_variant"`
	// scudo sanitizer, incompatible with asan, hwasan, tsan
	// This should not be used in Android 11+ : https://source.android.com/devices/tech/debug/scudo
	// deprecated
	Scudo *bool `android:"arch_variant"`
	// shadow-call-stack sanitizer, only available on arm64
	Scs *bool `android:"arch_variant"`
	// Memory-tagging, only available on arm64
	// if diag.memtag unset or false, enables async memory tagging
	Memtag_heap *bool `android:"arch_variant"`
	// Memory-tagging stack instrumentation, only available on arm64
	// Adds instrumentation to detect stack buffer overflows and use-after-scope using MTE.
	Memtag_stack *bool `android:"arch_variant"`

	// A modifier for ASAN and HWASAN for write only instrumentation
	Writeonly *bool `android:"arch_variant"`

	// Sanitizers to run in the diagnostic mode (as opposed to the release mode).
	// Replaces abort() on error with a human-readable error message.
	// Address and Thread sanitizers always run in diagnostic mode.
	Diag struct {
		// Undefined behavior sanitizer, diagnostic mode
		Undefined *bool `android:"arch_variant"`
		// cfi sanitizer, diagnostic mode, incompatible with asan, hwasan, fuzzer, or Darwin
		Cfi *bool `android:"arch_variant"`
		// signed/unsigned integer overflow sanitizer, diagnostic mode, incompatible with Darwin.
		Integer_overflow *bool `android:"arch_variant"`
		// Memory-tagging, only available on arm64
		// requires sanitizer.memtag: true
		// if set, enables sync memory tagging
		Memtag_heap *bool `android:"arch_variant"`
		// List of specific undefined behavior sanitizers to enable in diagnostic mode
		Misc_undefined []string `android:"arch_variant"`
		// List of sanitizers to pass to -fno-sanitize-recover
		// results in only the first detected error for these sanitizers being reported and program then
		// exits with a non-zero exit code.
		No_recover []string `android:"arch_variant"`
	} `android:"arch_variant"`

	// Sanitizers to run with flag configuration specified
	Config struct {
		// Enables CFI support flags for assembly-heavy libraries
		Cfi_assembly_support *bool `android:"arch_variant"`
	} `android:"arch_variant"`

	// List of sanitizers to pass to -fsanitize-recover
	// allows execution to continue for these sanitizers to detect multiple errors rather than only
	// the first one
	Recover []string

	// value to pass to -fsanitize-blacklist
	Blocklist *string
}

type SanitizeProperties struct {
	Sanitize          SanitizeUserProps `android:"arch_variant"`
	SanitizerEnabled  bool              `blueprint:"mutated"`
	MinimalRuntimeDep bool              `blueprint:"mutated"`
	BuiltinsDep       bool              `blueprint:"mutated"`
	UbsanRuntimeDep   bool              `blueprint:"mutated"`
	InSanitizerDir    bool              `blueprint:"mutated"`
	Sanitizers        []string          `blueprint:"mutated"`
	DiagSanitizers    []string          `blueprint:"mutated"`
}

type sanitize struct {
	Properties SanitizeProperties
}

// Mark this tag with a check to see if apex dependency check should be skipped
func (t libraryDependencyTag) SkipApexAllowedDependenciesCheck() bool {
	return t.skipApexAllowedDependenciesCheck
}

var _ android.SkipApexAllowedDependenciesCheck = (*libraryDependencyTag)(nil)

func init() {
	android.RegisterMakeVarsProvider(pctx, cfiMakeVarsProvider)
	android.RegisterMakeVarsProvider(pctx, hwasanMakeVarsProvider)
}

func (sanitize *sanitize) props() []interface{} {
	return []interface{}{&sanitize.Properties}
}

func (sanitize *sanitize) begin(ctx BaseModuleContext) {
	s := &sanitize.Properties.Sanitize

	// Don't apply sanitizers to NDK code.
	if ctx.useSdk() {
		s.Never = BoolPtr(true)
	}

	// Never always wins.
	if Bool(s.Never) {
		return
	}

	// cc_test targets default to SYNC MemTag unless explicitly set to ASYNC (via diag: {memtag_heap: false}).
	if ctx.testBinary() {
		if s.Memtag_heap == nil {
			s.Memtag_heap = proptools.BoolPtr(true)
		}
		if s.Diag.Memtag_heap == nil {
			s.Diag.Memtag_heap = proptools.BoolPtr(true)
		}
	}

	var globalSanitizers []string
	var globalSanitizersDiag []string

	if ctx.Host() {
		if !ctx.Windows() {
			globalSanitizers = ctx.Config().SanitizeHost()
		}
	} else {
		arches := ctx.Config().SanitizeDeviceArch()
		if len(arches) == 0 || inList(ctx.Arch().ArchType.Name, arches) {
			globalSanitizers = ctx.Config().SanitizeDevice()
			globalSanitizersDiag = ctx.Config().SanitizeDeviceDiag()
		}
	}

	if len(globalSanitizers) > 0 {
		var found bool
		if found, globalSanitizers = removeFromList("undefined", globalSanitizers); found && s.All_undefined == nil {
			s.All_undefined = proptools.BoolPtr(true)
		}

		if found, globalSanitizers = removeFromList("default-ub", globalSanitizers); found && s.Undefined == nil {
			s.Undefined = proptools.BoolPtr(true)
		}

		if found, globalSanitizers = removeFromList("address", globalSanitizers); found && s.Address == nil {
			s.Address = proptools.BoolPtr(true)
		}

		if found, globalSanitizers = removeFromList("thread", globalSanitizers); found && s.Thread == nil {
			s.Thread = proptools.BoolPtr(true)
		}

		if found, globalSanitizers = removeFromList("fuzzer", globalSanitizers); found && s.Fuzzer == nil {
			s.Fuzzer = proptools.BoolPtr(true)
		}

		if found, globalSanitizers = removeFromList("safe-stack", globalSanitizers); found && s.Safestack == nil {
			s.Safestack = proptools.BoolPtr(true)
		}

		if found, globalSanitizers = removeFromList("cfi", globalSanitizers); found && s.Cfi == nil {
			if !ctx.Config().CFIDisabledForPath(ctx.ModuleDir()) {
				s.Cfi = proptools.BoolPtr(true)
			}
		}

		// Global integer_overflow builds do not support static libraries.
		if found, globalSanitizers = removeFromList("integer_overflow", globalSanitizers); found && s.Integer_overflow == nil {
			if !ctx.Config().IntegerOverflowDisabledForPath(ctx.ModuleDir()) && !ctx.static() {
				s.Integer_overflow = proptools.BoolPtr(true)
			}
		}

		if found, globalSanitizers = removeFromList("scudo", globalSanitizers); found && s.Scudo == nil {
			s.Scudo = proptools.BoolPtr(true)
		}

		if found, globalSanitizers = removeFromList("hwaddress", globalSanitizers); found && s.Hwaddress == nil {
			s.Hwaddress = proptools.BoolPtr(true)
		}

		if found, globalSanitizers = removeFromList("writeonly", globalSanitizers); found && s.Writeonly == nil {
			// Hwaddress and Address are set before, so we can check them here
			// If they aren't explicitly set in the blueprint/SANITIZE_(HOST|TARGET), they would be nil instead of false
			if s.Address == nil && s.Hwaddress == nil {
				ctx.ModuleErrorf("writeonly modifier cannot be used without 'address' or 'hwaddress'")
			}
			s.Writeonly = proptools.BoolPtr(true)
		}
		if found, globalSanitizers = removeFromList("memtag_heap", globalSanitizers); found && s.Memtag_heap == nil {
			if !ctx.Config().MemtagHeapDisabledForPath(ctx.ModuleDir()) {
				s.Memtag_heap = proptools.BoolPtr(true)
			}
		}

		if found, globalSanitizers = removeFromList("memtag_stack", globalSanitizers); found && s.Memtag_stack == nil {
			s.Memtag_stack = proptools.BoolPtr(true)
		}

		if len(globalSanitizers) > 0 {
			ctx.ModuleErrorf("unknown global sanitizer option %s", globalSanitizers[0])
		}

		// Global integer_overflow builds do not support static library diagnostics.
		if found, globalSanitizersDiag = removeFromList("integer_overflow", globalSanitizersDiag); found &&
			s.Diag.Integer_overflow == nil && Bool(s.Integer_overflow) && !ctx.static() {
			s.Diag.Integer_overflow = proptools.BoolPtr(true)
		}

		if found, globalSanitizersDiag = removeFromList("cfi", globalSanitizersDiag); found &&
			s.Diag.Cfi == nil && Bool(s.Cfi) {
			s.Diag.Cfi = proptools.BoolPtr(true)
		}

		if found, globalSanitizersDiag = removeFromList("memtag_heap", globalSanitizersDiag); found &&
			s.Diag.Memtag_heap == nil && Bool(s.Memtag_heap) {
			s.Diag.Memtag_heap = proptools.BoolPtr(true)
		}

		if len(globalSanitizersDiag) > 0 {
			ctx.ModuleErrorf("unknown global sanitizer diagnostics option %s", globalSanitizersDiag[0])
		}
	}

	// Enable Memtag for all components in the include paths (for Aarch64 only)
	if ctx.Arch().ArchType == android.Arm64 && ctx.toolchain().Bionic() {
		if ctx.Config().MemtagHeapSyncEnabledForPath(ctx.ModuleDir()) {
			if s.Memtag_heap == nil {
				s.Memtag_heap = proptools.BoolPtr(true)
			}
			if s.Diag.Memtag_heap == nil {
				s.Diag.Memtag_heap = proptools.BoolPtr(true)
			}
		} else if ctx.Config().MemtagHeapAsyncEnabledForPath(ctx.ModuleDir()) {
			if s.Memtag_heap == nil {
				s.Memtag_heap = proptools.BoolPtr(true)
			}
		}
	}

	if s.Integer_overflow == nil && ctx.Config().IntegerOverflowEnabledForPath(ctx.ModuleDir()) && ctx.Arch().ArchType == android.Arm64 {
		s.Integer_overflow = proptools.BoolPtr(true)
	}

	if ctx.Config().BoundSanitizerEnabledForPath(ctx.ModuleDir()) && ctx.Arch().ArchType == android.Arm64 {
		s.Misc_undefined = append(s.Misc_undefined, "bounds")
	}

	if ctx.Config().BoundSanitizerDisabledForPath(ctx.ModuleDir()) && ctx.Arch().ArchType == android.Arm64 {
		indx := indexList("bounds", s.Misc_undefined)
		if indexList("bounds", s.Misc_undefined) != -1 {
			s.Misc_undefined = append(s.Misc_undefined[0:indx], s.Misc_undefined[indx+1:]...)
		}
	}

	// Disable integer-overflow in exclude path
	if ctx.Config().IntegerOverflowDisabledForPath(ctx.ModuleDir()) && ctx.Arch().ArchType == android.Arm64 {
		indx := indexList("signed-integer-overflow", s.Misc_undefined)
		if indexList("signed-integer-overflow", s.Misc_undefined) != -1 {
			s.Misc_undefined = append(s.Misc_undefined[0:indx], s.Misc_undefined[indx+1:]...)
		}

		indx = indexList("unsigned-integer-overflow", s.Misc_undefined)
		if indexList("unsigned-integer-overflow", s.Misc_undefined) != -1 {
			s.Misc_undefined = append(s.Misc_undefined[0:indx], s.Misc_undefined[indx+1:]...)
		}
		s.Integer_overflow = nil
	}

	// Enable CFI for non-host components in the include paths
	if s.Cfi == nil && ctx.Config().CFIEnabledForPath(ctx.ModuleDir()) && !ctx.Host() {
		s.Cfi = proptools.BoolPtr(true)
		if inList("cfi", ctx.Config().SanitizeDeviceDiag()) {
			s.Diag.Cfi = proptools.BoolPtr(true)
		}
	}
	// Disable CFI for all component in the exclude path (for Aarch64 only)
	if ctx.Config().CFIDisabledForPath(ctx.ModuleDir()) && ctx.Arch().ArchType == android.Arm64 {
		s.Cfi = nil
		if inList("cfi", ctx.Config().SanitizeDeviceDiag()) {
			s.Diag.Cfi = nil
		}
	}

	// Is CFI actually enabled?
	if !ctx.Config().EnableCFI() {
		s.Cfi = nil
		s.Diag.Cfi = nil
	}

	// HWASan requires AArch64 hardware feature (top-byte-ignore).
	if ctx.Arch().ArchType != android.Arm64 || !ctx.toolchain().Bionic() {
		s.Hwaddress = nil
	}

	// SCS is only implemented on AArch64.
	if ctx.Arch().ArchType != android.Arm64 || !ctx.toolchain().Bionic() {
		s.Scs = nil
	}

	// Memtag_heap is only implemented on AArch64.
	// Memtag ABI is Android specific for now, so disable for host.
	if ctx.Arch().ArchType != android.Arm64 || !ctx.toolchain().Bionic() || ctx.Host() {
		s.Memtag_heap = nil
		s.Memtag_stack = nil
	}

	// Also disable CFI if ASAN is enabled.
	if Bool(s.Address) || Bool(s.Hwaddress) {
		s.Cfi = nil
		s.Diag.Cfi = nil
		// HWASAN and ASAN win against MTE.
		s.Memtag_heap = nil
		s.Memtag_stack = nil
	}

	// Disable sanitizers that depend on the UBSan runtime for windows/darwin builds.
	if !ctx.Os().Linux() {
		s.Cfi = nil
		s.Diag.Cfi = nil
		s.Misc_undefined = nil
		s.Undefined = nil
		s.All_undefined = nil
		s.Integer_overflow = nil
	}

	// Disable CFI for musl
	if ctx.toolchain().Musl() {
		s.Cfi = nil
		s.Diag.Cfi = nil
	}

	// Also disable CFI for VNDK variants of components
	if ctx.isVndk() && ctx.useVndk() {
		if ctx.static() {
			// Cfi variant for static vndk should be captured as vendor snapshot,
			// so don't strictly disable Cfi.
			s.Cfi = nil
			s.Diag.Cfi = nil
		} else {
			s.Cfi = nil
			s.Diag.Cfi = nil
		}
	}

	// HWASan ramdisk (which is built from recovery) goes over some bootloader limit.
	// Keep libc instrumented so that ramdisk / vendor_ramdisk / recovery can run hwasan-instrumented code if necessary.
	if (ctx.inRamdisk() || ctx.inVendorRamdisk() || ctx.inRecovery()) && !strings.HasPrefix(ctx.ModuleDir(), "bionic/libc") {
		s.Hwaddress = nil
	}

	if ctx.staticBinary() {
		s.Address = nil
		s.Fuzzer = nil
		s.Thread = nil
	}

	if Bool(s.All_undefined) {
		s.Undefined = nil
	}

	if !ctx.toolchain().Is64Bit() {
		// TSAN and SafeStack are not supported on 32-bit architectures
		s.Thread = nil
		s.Safestack = nil
		// TODO(ccross): error for compile_multilib = "32"?
	}

	if ctx.Os() != android.Windows && (Bool(s.All_undefined) || Bool(s.Undefined) || Bool(s.Address) || Bool(s.Thread) ||
		Bool(s.Fuzzer) || Bool(s.Safestack) || Bool(s.Cfi) || Bool(s.Integer_overflow) || len(s.Misc_undefined) > 0 ||
		Bool(s.Scudo) || Bool(s.Hwaddress) || Bool(s.Scs) || Bool(s.Memtag_heap) || Bool(s.Memtag_stack)) {
		sanitize.Properties.SanitizerEnabled = true
	}

	// Disable Scudo if ASan or TSan is enabled, or if it's disabled globally.
	if Bool(s.Address) || Bool(s.Thread) || Bool(s.Hwaddress) || ctx.Config().DisableScudo() {
		s.Scudo = nil
	}

	if Bool(s.Hwaddress) {
		s.Address = nil
		s.Thread = nil
	}

	// TODO(b/131771163): CFI transiently depends on LTO, and thus Fuzzer is
	// mutually incompatible.
	if Bool(s.Fuzzer) {
		s.Cfi = nil
	}
}

func toDisableImplicitIntegerChange(flags []string) bool {
	// Returns true if any flag is fsanitize*integer, and there is
	// no explicit flag about sanitize=implicit-integer-sign-change.
	for _, f := range flags {
		if strings.Contains(f, "sanitize=implicit-integer-sign-change") {
			return false
		}
	}
	for _, f := range flags {
		if strings.HasPrefix(f, "-fsanitize") && strings.Contains(f, "integer") {
			return true
		}
	}
	return false
}

func toDisableUnsignedShiftBaseChange(flags []string) bool {
	// Returns true if any flag is fsanitize*integer, and there is
	// no explicit flag about sanitize=unsigned-shift-base.
	for _, f := range flags {
		if strings.Contains(f, "sanitize=unsigned-shift-base") {
			return false
		}
	}
	for _, f := range flags {
		if strings.HasPrefix(f, "-fsanitize") && strings.Contains(f, "integer") {
			return true
		}
	}
	return false
}

func (sanitize *sanitize) flags(ctx ModuleContext, flags Flags) Flags {
	if !sanitize.Properties.SanitizerEnabled && !sanitize.Properties.UbsanRuntimeDep {
		return flags
	}

	if Bool(sanitize.Properties.Sanitize.Address) {
		if ctx.Arch().ArchType == android.Arm {
			// Frame pointer based unwinder in ASan requires ARM frame setup.
			// TODO: put in flags?
			flags.RequiredInstructionSet = "arm"
		}
		flags.Local.CFlags = append(flags.Local.CFlags, asanCflags...)
		flags.Local.LdFlags = append(flags.Local.LdFlags, asanLdflags...)

		if Bool(sanitize.Properties.Sanitize.Writeonly) {
			flags.Local.CFlags = append(flags.Local.CFlags, "-mllvm", "-asan-instrument-reads=0")
		}

		if ctx.Host() {
			// -nodefaultlibs (provided with libc++) prevents the driver from linking
			// libraries needed with -fsanitize=address. http://b/18650275 (WAI)
			flags.Local.LdFlags = append(flags.Local.LdFlags, "-Wl,--no-as-needed")
		} else {
			flags.Local.CFlags = append(flags.Local.CFlags, "-mllvm", "-asan-globals=0")
			if ctx.bootstrap() {
				flags.DynamicLinker = "/system/bin/bootstrap/linker_asan"
			} else {
				flags.DynamicLinker = "/system/bin/linker_asan"
			}
			if flags.Toolchain.Is64Bit() {
				flags.DynamicLinker += "64"
			}
		}
	}

	if Bool(sanitize.Properties.Sanitize.Hwaddress) {
		flags.Local.CFlags = append(flags.Local.CFlags, hwasanCflags...)

		for _, flag := range hwasanCommonflags {
			flags.Local.CFlags = append(flags.Local.CFlags, "-mllvm", flag)
		}
		for _, flag := range hwasanCommonflags {
			flags.Local.LdFlags = append(flags.Local.LdFlags, "-Wl,-mllvm,"+flag)
		}

		if Bool(sanitize.Properties.Sanitize.Writeonly) {
			flags.Local.CFlags = append(flags.Local.CFlags, "-mllvm", "-hwasan-instrument-reads=0")
		}
	}

	if Bool(sanitize.Properties.Sanitize.Fuzzer) {
		flags.Local.CFlags = append(flags.Local.CFlags, "-fsanitize=fuzzer-no-link")

		// TODO(b/131771163): LTO and Fuzzer support is mutually incompatible.
		_, flags.Local.LdFlags = removeFromList("-flto", flags.Local.LdFlags)
		_, flags.Local.CFlags = removeFromList("-flto", flags.Local.CFlags)
		flags.Local.LdFlags = append(flags.Local.LdFlags, "-fno-lto")
		flags.Local.CFlags = append(flags.Local.CFlags, "-fno-lto")

		// TODO(b/142430592): Upstream linker scripts for sanitizer runtime libraries
		// discard the sancov_lowest_stack symbol, because it's emulated TLS (and thus
		// doesn't match the linker script due to the "__emutls_v." prefix).
		flags.Local.LdFlags = append(flags.Local.LdFlags, "-fno-sanitize-coverage=stack-depth")
		flags.Local.CFlags = append(flags.Local.CFlags, "-fno-sanitize-coverage=stack-depth")

		// Disable fortify for fuzzing builds. Generally, we'll be building with
		// UBSan or ASan here and the fortify checks pollute the stack traces.
		flags.Local.CFlags = append(flags.Local.CFlags, "-U_FORTIFY_SOURCE")

		// Build fuzzer-sanitized libraries with an $ORIGIN DT_RUNPATH. Android's
		// linker uses DT_RUNPATH, not DT_RPATH. When we deploy cc_fuzz targets and
		// their libraries to /data/fuzz/<arch>/lib, any transient shared library gets
		// the DT_RUNPATH from the shared library above it, and not the executable,
		// meaning that the lookup falls back to the system. Adding the $ORIGIN to the
		// DT_RUNPATH here means that transient shared libraries can be found
		// colocated with their parents.
		flags.Local.LdFlags = append(flags.Local.LdFlags, `-Wl,-rpath,\$$ORIGIN`)
	}

	if Bool(sanitize.Properties.Sanitize.Cfi) {
		if ctx.Arch().ArchType == android.Arm {
			// __cfi_check needs to be built as Thumb (see the code in linker_cfi.cpp). LLVM is not set up
			// to do this on a function basis, so force Thumb on the entire module.
			flags.RequiredInstructionSet = "thumb"
		}

		flags.Local.CFlags = append(flags.Local.CFlags, cfiCflags...)
		flags.Local.AsFlags = append(flags.Local.AsFlags, cfiAsflags...)
		if Bool(sanitize.Properties.Sanitize.Config.Cfi_assembly_support) {
			flags.Local.CFlags = append(flags.Local.CFlags, "-fno-sanitize-cfi-canonical-jump-tables")
		}
		// Only append the default visibility flag if -fvisibility has not already been set
		// to hidden.
		if !inList("-fvisibility=hidden", flags.Local.CFlags) {
			flags.Local.CFlags = append(flags.Local.CFlags, "-fvisibility=default")
		}
		flags.Local.LdFlags = append(flags.Local.LdFlags, cfiLdflags...)

		if ctx.staticBinary() {
			_, flags.Local.CFlags = removeFromList("-fsanitize-cfi-cross-dso", flags.Local.CFlags)
			_, flags.Local.LdFlags = removeFromList("-fsanitize-cfi-cross-dso", flags.Local.LdFlags)
		}
	}

	if Bool(sanitize.Properties.Sanitize.Memtag_stack) {
		flags.Local.CFlags = append(flags.Local.CFlags, memtagStackCommonFlags...)
		// TODO(fmayer): remove -Wno-error once https://reviews.llvm.org/D127917 is in Android toolchain.
		flags.Local.CFlags = append(flags.Local.CFlags, "-Wno-error=frame-larger-than")
		flags.Local.AsFlags = append(flags.Local.AsFlags, memtagStackCommonFlags...)
		flags.Local.LdFlags = append(flags.Local.LdFlags, memtagStackCommonFlags...)
		// This works around LLD complaining about the stack frame size.
		// TODO(fmayer): remove once https://reviews.llvm.org/D127917 is in Android toolchain.
		flags.Local.LdFlags = append(flags.Local.LdFlags, "-Wl,--no-fatal-warnings")
	}

	// TODO(b/249094918) re-enable after clang version brought back in-line with upstream
	/*
		if (Bool(sanitize.Properties.Sanitize.Memtag_heap) || Bool(sanitize.Properties.Sanitize.Memtag_stack)) && ctx.binary() {
			if Bool(sanitize.Properties.Sanitize.Diag.Memtag_heap) {
				flags.Local.LdFlags = append(flags.Local.LdFlags, "-fsanitize-memtag-mode=sync")
			} else {
				flags.Local.LdFlags = append(flags.Local.LdFlags, "-fsanitize-memtag-mode=async")
			}
		}
	*/

	if Bool(sanitize.Properties.Sanitize.Integer_overflow) {
		flags.Local.CFlags = append(flags.Local.CFlags, intOverflowCflags...)
	}

	if len(sanitize.Properties.Sanitizers) > 0 {
		sanitizeArg := "-fsanitize=" + strings.Join(sanitize.Properties.Sanitizers, ",")
		flags.Local.CFlags = append(flags.Local.CFlags, sanitizeArg)
		flags.Local.AsFlags = append(flags.Local.AsFlags, sanitizeArg)
		flags.Local.LdFlags = append(flags.Local.LdFlags, sanitizeArg)

		if ctx.toolchain().Bionic() || ctx.toolchain().Musl() {
			// Bionic and musl sanitizer runtimes have already been added as dependencies so that
			// the right variant of the runtime will be used (with the "-android" or "-musl"
			// suffixes), so don't let clang the runtime library.
			flags.Local.LdFlags = append(flags.Local.LdFlags, "-fno-sanitize-link-runtime")
		} else {
			// Host sanitizers only link symbols in the final executable, so
			// there will always be undefined symbols in intermediate libraries.
			_, flags.Global.LdFlags = removeFromList("-Wl,--no-undefined", flags.Global.LdFlags)
		}

		if !ctx.toolchain().Bionic() {
			// non-Bionic toolchain prebuilts are missing UBSan's vptr and function san.
			// Musl toolchain prebuilts have vptr and function sanitizers, but enabling them
			// implicitly enables RTTI which causes RTTI mismatch issues with dependencies.

			flags.Local.CFlags = append(flags.Local.CFlags, "-fno-sanitize=vptr,function")
		}

		if Bool(sanitize.Properties.Sanitize.Fuzzer) {
			// When fuzzing, we wish to crash with diagnostics on any bug.
			flags.Local.CFlags = append(flags.Local.CFlags, "-fno-sanitize-trap=all", "-fno-sanitize-recover=all")
		} else if ctx.Host() {
			flags.Local.CFlags = append(flags.Local.CFlags, "-fno-sanitize-recover=all")
		} else {
			flags.Local.CFlags = append(flags.Local.CFlags, "-fsanitize-trap=all", "-ftrap-function=abort")
		}

		if enableMinimalRuntime(sanitize) {
			flags.Local.CFlags = append(flags.Local.CFlags, strings.Join(minimalRuntimeFlags, " "))
		}

		// http://b/119329758, Android core does not boot up with this sanitizer yet.
		if toDisableImplicitIntegerChange(flags.Local.CFlags) {
			flags.Local.CFlags = append(flags.Local.CFlags, "-fno-sanitize=implicit-integer-sign-change")
		}
		// http://b/171275751, Android doesn't build with this sanitizer yet.
		// KEYSTONE(I31a5c1934a7faae000833f849a7c022b0e45a6f4,b/178857879)
		if toDisableUnsignedShiftBaseChange(flags.Local.CFlags) && !flags.Sdclang {
			flags.Local.CFlags = append(flags.Local.CFlags, "-fno-sanitize=unsigned-shift-base")
		}
	}

	if len(sanitize.Properties.DiagSanitizers) > 0 {
		flags.Local.CFlags = append(flags.Local.CFlags, "-fno-sanitize-trap="+strings.Join(sanitize.Properties.DiagSanitizers, ","))
	}
	// FIXME: enable RTTI if diag + (cfi or vptr)

	if sanitize.Properties.Sanitize.Recover != nil {
		flags.Local.CFlags = append(flags.Local.CFlags, "-fsanitize-recover="+
			strings.Join(sanitize.Properties.Sanitize.Recover, ","))
	}

	if sanitize.Properties.Sanitize.Diag.No_recover != nil {
		flags.Local.CFlags = append(flags.Local.CFlags, "-fno-sanitize-recover="+
			strings.Join(sanitize.Properties.Sanitize.Diag.No_recover, ","))
	}

	blocklist := android.OptionalPathForModuleSrc(ctx, sanitize.Properties.Sanitize.Blocklist)
	if blocklist.Valid() {
		flags.Local.CFlags = append(flags.Local.CFlags, "-fsanitize-blacklist="+blocklist.String())
		flags.CFlagsDeps = append(flags.CFlagsDeps, blocklist.Path())
	}

	return flags
}

func (sanitize *sanitize) AndroidMkEntries(ctx AndroidMkContext, entries *android.AndroidMkEntries) {
	// Add a suffix for cfi/hwasan/scs-enabled static/header libraries to allow surfacing
	// both the sanitized and non-sanitized variants to make without a name conflict.
	if entries.Class == "STATIC_LIBRARIES" || entries.Class == "HEADER_LIBRARIES" {
		if Bool(sanitize.Properties.Sanitize.Cfi) {
			entries.SubName += ".cfi"
		}
		if Bool(sanitize.Properties.Sanitize.Hwaddress) {
			entries.SubName += ".hwasan"
		}
		if Bool(sanitize.Properties.Sanitize.Scs) {
			entries.SubName += ".scs"
		}
	}
}

func (sanitize *sanitize) inSanitizerDir() bool {
	return sanitize.Properties.InSanitizerDir
}

// getSanitizerBoolPtr returns the SanitizerTypes associated bool pointer from SanitizeProperties.
func (sanitize *sanitize) getSanitizerBoolPtr(t SanitizerType) *bool {
	switch t {
	case Asan:
		return sanitize.Properties.Sanitize.Address
	case Hwasan:
		return sanitize.Properties.Sanitize.Hwaddress
	case tsan:
		return sanitize.Properties.Sanitize.Thread
	case intOverflow:
		return sanitize.Properties.Sanitize.Integer_overflow
	case cfi:
		return sanitize.Properties.Sanitize.Cfi
	case scs:
		return sanitize.Properties.Sanitize.Scs
	case Memtag_heap:
		return sanitize.Properties.Sanitize.Memtag_heap
	case Memtag_stack:
		return sanitize.Properties.Sanitize.Memtag_stack
	case Fuzzer:
		return sanitize.Properties.Sanitize.Fuzzer
	default:
		panic(fmt.Errorf("unknown SanitizerType %d", t))
	}
}

// isUnsanitizedVariant returns true if no sanitizers are enabled.
func (sanitize *sanitize) isUnsanitizedVariant() bool {
	return !sanitize.isSanitizerEnabled(Asan) &&
		!sanitize.isSanitizerEnabled(Hwasan) &&
		!sanitize.isSanitizerEnabled(tsan) &&
		!sanitize.isSanitizerEnabled(cfi) &&
		!sanitize.isSanitizerEnabled(scs) &&
		!sanitize.isSanitizerEnabled(Memtag_heap) &&
		!sanitize.isSanitizerEnabled(Memtag_stack) &&
		!sanitize.isSanitizerEnabled(Fuzzer)
}

// isVariantOnProductionDevice returns true if variant is for production devices (no non-production sanitizers enabled).
func (sanitize *sanitize) isVariantOnProductionDevice() bool {
	return !sanitize.isSanitizerEnabled(Asan) &&
		!sanitize.isSanitizerEnabled(Hwasan) &&
		!sanitize.isSanitizerEnabled(tsan) &&
		!sanitize.isSanitizerEnabled(Fuzzer)
}

func (sanitize *sanitize) SetSanitizer(t SanitizerType, b bool) {
	bPtr := proptools.BoolPtr(b)
	if !b {
		bPtr = nil
	}
	switch t {
	case Asan:
		sanitize.Properties.Sanitize.Address = bPtr
	case Hwasan:
		sanitize.Properties.Sanitize.Hwaddress = bPtr
	case tsan:
		sanitize.Properties.Sanitize.Thread = bPtr
	case intOverflow:
		sanitize.Properties.Sanitize.Integer_overflow = bPtr
	case cfi:
		sanitize.Properties.Sanitize.Cfi = bPtr
	case scs:
		sanitize.Properties.Sanitize.Scs = bPtr
	case Memtag_heap:
		sanitize.Properties.Sanitize.Memtag_heap = bPtr
	case Memtag_stack:
		sanitize.Properties.Sanitize.Memtag_stack = bPtr
	case Fuzzer:
		sanitize.Properties.Sanitize.Fuzzer = bPtr
	default:
		panic(fmt.Errorf("unknown SanitizerType %d", t))
	}
	if b {
		sanitize.Properties.SanitizerEnabled = true
	}
}

// Check if the sanitizer is explicitly disabled (as opposed to nil by
// virtue of not being set).
func (sanitize *sanitize) isSanitizerExplicitlyDisabled(t SanitizerType) bool {
	if sanitize == nil {
		return false
	}

	sanitizerVal := sanitize.getSanitizerBoolPtr(t)
	return sanitizerVal != nil && *sanitizerVal == false
}

// There isn't an analog of the method above (ie:isSanitizerExplicitlyEnabled)
// because enabling a sanitizer either directly (via the blueprint) or
// indirectly (via a mutator) sets the bool ptr to true, and you can't
// distinguish between the cases. It isn't needed though - both cases can be
// treated identically.
func (sanitize *sanitize) isSanitizerEnabled(t SanitizerType) bool {
	if sanitize == nil {
		return false
	}

	sanitizerVal := sanitize.getSanitizerBoolPtr(t)
	return sanitizerVal != nil && *sanitizerVal == true
}

// IsSanitizableDependencyTag returns true if the dependency tag is sanitizable.
func IsSanitizableDependencyTag(tag blueprint.DependencyTag) bool {
	switch t := tag.(type) {
	case dependencyTag:
		return t == reuseObjTag || t == objDepTag
	case libraryDependencyTag:
		return true
	default:
		return false
	}
}

func (m *Module) SanitizableDepTagChecker() SantizableDependencyTagChecker {
	return IsSanitizableDependencyTag
}

// Determines if the current module is a static library going to be captured
// as vendor snapshot. Such modules must create both cfi and non-cfi variants,
// except for ones which explicitly disable cfi.
func needsCfiForVendorSnapshot(mctx android.BaseModuleContext) bool {
	if snapshot.IsVendorProprietaryModule(mctx) {
		return false
	}

	c := mctx.Module().(PlatformSanitizeable)

	if !c.InVendor() {
		return false
	}

	if !c.StaticallyLinked() {
		return false
	}

	if c.IsPrebuilt() {
		return false
	}

	if !c.SanitizerSupported(cfi) {
		return false
	}

	return c.SanitizePropDefined() &&
		!c.SanitizeNever() &&
		!c.IsSanitizerExplicitlyDisabled(cfi)
}

type sanitizerSplitMutator struct {
	sanitizer SanitizerType
}

// If an APEX is sanitized or not depends on whether it contains at least one
// sanitized module. Transition mutators cannot propagate information up the
// dependency graph this way, so we need an auxiliary mutator to do so.
func (s *sanitizerSplitMutator) markSanitizableApexesMutator(ctx android.TopDownMutatorContext) {
	if sanitizeable, ok := ctx.Module().(Sanitizeable); ok {
		enabled := sanitizeable.IsSanitizerEnabled(ctx.Config(), s.sanitizer.name())
		ctx.VisitDirectDeps(func(dep android.Module) {
			if c, ok := dep.(*Module); ok && c.sanitize.isSanitizerEnabled(s.sanitizer) {
				enabled = true
			}
		})

		if enabled {
			sanitizeable.EnableSanitizer(s.sanitizer.name())
		}
	}
}

func (s *sanitizerSplitMutator) Split(ctx android.BaseModuleContext) []string {
	if c, ok := ctx.Module().(PlatformSanitizeable); ok && c.SanitizePropDefined() {
		if s.sanitizer == cfi && needsCfiForVendorSnapshot(ctx) {
			return []string{"", s.sanitizer.variationName()}
		}

		// If the given sanitizer is not requested in the .bp file for a module, it
		// won't automatically build the sanitized variation.
		if !c.IsSanitizerEnabled(s.sanitizer) {
			return []string{""}
		}

		if c.Binary() {
			// If a sanitizer is enabled for a binary, we do not build the version
			// without the sanitizer
			return []string{s.sanitizer.variationName()}
		} else if c.StaticallyLinked() || c.Header() {
			// For static libraries, we build both versions. Some Make modules
			// apparently depend on this behavior.
			return []string{"", s.sanitizer.variationName()}
		} else {
			// We only build the requested variation of dynamic libraries
			return []string{s.sanitizer.variationName()}
		}
	}

	if _, ok := ctx.Module().(JniSanitizeable); ok {
		// TODO: this should call into JniSanitizable.IsSanitizerEnabledForJni but
		// that is short-circuited for now
		return []string{""}
	}

	// If an APEX has a sanitized dependency, we build the APEX in the sanitized
	// variation. This is useful because such APEXes require extra dependencies.
	if sanitizeable, ok := ctx.Module().(Sanitizeable); ok {
		enabled := sanitizeable.IsSanitizerEnabled(ctx.Config(), s.sanitizer.name())
		if enabled {
			return []string{s.sanitizer.variationName()}
		} else {
			return []string{""}
		}
	}

	if c, ok := ctx.Module().(*Module); ok {
		//TODO: When Rust modules have vendor support, enable this path for PlatformSanitizeable

		// Check if it's a snapshot module supporting sanitizer
		if ss, ok := c.linker.(snapshotSanitizer); ok && ss.isSanitizerEnabled(s.sanitizer) {
			return []string{"", s.sanitizer.variationName()}
		} else {
			return []string{""}
		}
	}

	return []string{""}
}

func (s *sanitizerSplitMutator) OutgoingTransition(ctx android.OutgoingTransitionContext, sourceVariation string) string {
	if c, ok := ctx.Module().(PlatformSanitizeable); ok {
		if !c.SanitizableDepTagChecker()(ctx.DepTag()) {
			// If the dependency is through a non-sanitizable tag, use the
			// non-sanitized variation
			return ""
		}

		return sourceVariation
	} else if _, ok := ctx.Module().(JniSanitizeable); ok {
		// TODO: this should call into JniSanitizable.IsSanitizerEnabledForJni but
		// that is short-circuited for now
		return ""
	} else {
		// Otherwise, do not rock the boat.
		return sourceVariation
	}
}

func (s *sanitizerSplitMutator) IncomingTransition(ctx android.IncomingTransitionContext, incomingVariation string) string {
	if d, ok := ctx.Module().(PlatformSanitizeable); ok {
		if dm, ok := ctx.Module().(*Module); ok {
			if ss, ok := dm.linker.(snapshotSanitizer); ok && ss.isSanitizerEnabled(s.sanitizer) {
				return incomingVariation
			}
		}

		if !d.SanitizePropDefined() ||
			d.SanitizeNever() ||
			d.IsSanitizerExplicitlyDisabled(s.sanitizer) ||
			!d.SanitizerSupported(s.sanitizer) {
			// If a module opts out of a sanitizer, use its non-sanitized variation
			return ""
		}

		// Binaries are always built in the variation they requested.
		if d.Binary() {
			if d.IsSanitizerEnabled(s.sanitizer) {
				return s.sanitizer.variationName()
			} else {
				return ""
			}
		}

		// If a shared library requests to be sanitized, it will be built for that
		// sanitizer. Otherwise, some sanitizers propagate through shared library
		// dependency edges, some do not.
		if !d.StaticallyLinked() && !d.Header() {
			if d.IsSanitizerEnabled(s.sanitizer) {
				return s.sanitizer.variationName()
			}

			if s.sanitizer == cfi || s.sanitizer == Hwasan || s.sanitizer == scs || s.sanitizer == Asan {
				return ""
			}
		}

		// Static and header libraries inherit whether they are sanitized from the
		// module they are linked into
		return incomingVariation
	} else if d, ok := ctx.Module().(Sanitizeable); ok {
		// If an APEX contains a sanitized module, it will be built in the variation
		// corresponding to that sanitizer.
		enabled := d.IsSanitizerEnabled(ctx.Config(), s.sanitizer.name())
		if enabled {
			return s.sanitizer.variationName()
		}

		return incomingVariation
	}

	return ""
}

func (s *sanitizerSplitMutator) Mutate(mctx android.BottomUpMutatorContext, variationName string) {
	sanitizerVariation := variationName == s.sanitizer.variationName()

	if c, ok := mctx.Module().(PlatformSanitizeable); ok && c.SanitizePropDefined() {
		sanitizerEnabled := c.IsSanitizerEnabled(s.sanitizer)

		oneMakeVariation := false
		if c.StaticallyLinked() || c.Header() {
			if s.sanitizer != cfi && s.sanitizer != scs && s.sanitizer != Hwasan {
				// These sanitizers export only one variation to Make. For the rest,
				// Make targets can depend on both the sanitized and non-sanitized
				// versions.
				oneMakeVariation = true
			}
		} else if !c.Binary() {
			// Shared library. These are the sanitizers that do propagate through shared
			// library dependencies and therefore can cause multiple variations of a
			// shared library to be built.
			if s.sanitizer != cfi && s.sanitizer != Hwasan && s.sanitizer != scs && s.sanitizer != Asan {
				oneMakeVariation = true
			}
		}

		if oneMakeVariation {
			if sanitizerEnabled != sanitizerVariation {
				c.SetPreventInstall()
				c.SetHideFromMake()
			}
		}

		if sanitizerVariation {
			c.SetSanitizer(s.sanitizer, true)

			// CFI is incompatible with ASAN so disable it in ASAN variations
			if s.sanitizer.incompatibleWithCfi() {
				cfiSupported := mctx.Module().(PlatformSanitizeable).SanitizerSupported(cfi)
				if mctx.Device() && cfiSupported {
					c.SetSanitizer(cfi, false)
				}
			}

			// locate the asan libraries under /data/asan
			if !c.Binary() && !c.StaticallyLinked() && !c.Header() && mctx.Device() && s.sanitizer == Asan && sanitizerEnabled {
				c.SetInSanitizerDir()
			}

			if c.StaticallyLinked() && c.ExportedToMake() {
				if s.sanitizer == Hwasan {
					hwasanStaticLibs(mctx.Config()).add(c, c.Module().Name())
				} else if s.sanitizer == cfi {
					cfiStaticLibs(mctx.Config()).add(c, c.Module().Name())
				}
			}
		} else if c.IsSanitizerEnabled(s.sanitizer) {
			// Disable the sanitizer for the non-sanitized variation
			c.SetSanitizer(s.sanitizer, false)
		}
	} else if sanitizeable, ok := mctx.Module().(Sanitizeable); ok {
		// If an APEX has sanitized dependencies, it gets a few more dependencies
		if sanitizerVariation {
			sanitizeable.AddSanitizerDependencies(mctx, s.sanitizer.name())
		}
	} else if c, ok := mctx.Module().(*Module); ok {
		if ss, ok := c.linker.(snapshotSanitizer); ok && ss.isSanitizerEnabled(s.sanitizer) {
			c.linker.(snapshotSanitizer).setSanitizerVariation(s.sanitizer, sanitizerVariation)

			// Export the static lib name to make
			if c.static() && c.ExportedToMake() {
				if s.sanitizer == cfi {
					// use BaseModuleName which is the name for Make.
					cfiStaticLibs(mctx.Config()).add(c, c.BaseModuleName())
				}
			}
		}
	}
}

func (c *Module) SanitizeNever() bool {
	return Bool(c.sanitize.Properties.Sanitize.Never)
}

func (c *Module) IsSanitizerExplicitlyDisabled(t SanitizerType) bool {
	return c.sanitize.isSanitizerExplicitlyDisabled(t)
}

// Propagate the ubsan minimal runtime dependency when there are integer overflow sanitized static dependencies.
func sanitizerRuntimeDepsMutator(mctx android.TopDownMutatorContext) {
	// Change this to PlatformSanitizable when/if non-cc modules support ubsan sanitizers.
	if c, ok := mctx.Module().(*Module); ok && c.sanitize != nil {
		isSanitizableDependencyTag := c.SanitizableDepTagChecker()
		mctx.WalkDeps(func(child, parent android.Module) bool {
			if !isSanitizableDependencyTag(mctx.OtherModuleDependencyTag(child)) {
				return false
			}

			d, ok := child.(*Module)
			if !ok || !d.static() {
				return false
			}
			if d.sanitize != nil {
				if enableMinimalRuntime(d.sanitize) {
					// If a static dependency is built with the minimal runtime,
					// make sure we include the ubsan minimal runtime.
					c.sanitize.Properties.MinimalRuntimeDep = true
				} else if enableUbsanRuntime(d.sanitize) {
					// If a static dependency runs with full ubsan diagnostics,
					// make sure we include the ubsan runtime.
					c.sanitize.Properties.UbsanRuntimeDep = true
				}

				if c.sanitize.Properties.MinimalRuntimeDep &&
					c.sanitize.Properties.UbsanRuntimeDep {
					// both flags that this mutator might set are true, so don't bother recursing
					return false
				}

				if c.Os() == android.Linux {
					c.sanitize.Properties.BuiltinsDep = true
				}

				return true
			}

			if p, ok := d.linker.(*snapshotLibraryDecorator); ok {
				if Bool(p.properties.Sanitize_minimal_dep) {
					c.sanitize.Properties.MinimalRuntimeDep = true
				}
				if Bool(p.properties.Sanitize_ubsan_dep) {
					c.sanitize.Properties.UbsanRuntimeDep = true
				}
			}

			return false
		})
	}
}

// Add the dependency to the runtime library for each of the sanitizer variants
func sanitizerRuntimeMutator(mctx android.BottomUpMutatorContext) {
	if c, ok := mctx.Module().(*Module); ok && c.sanitize != nil {
		if !c.Enabled() {
			return
		}
		var sanitizers []string
		var diagSanitizers []string

		if Bool(c.sanitize.Properties.Sanitize.All_undefined) {
			sanitizers = append(sanitizers, "undefined")
		} else {
			if Bool(c.sanitize.Properties.Sanitize.Undefined) {
				sanitizers = append(sanitizers,
					"bool",
					"integer-divide-by-zero",
					"return",
					"returns-nonnull-attribute",
					"shift-exponent",
					"unreachable",
					"vla-bound",
					// TODO(danalbert): The following checks currently have compiler performance issues.
					//"alignment",
					//"bounds",
					//"enum",
					//"float-cast-overflow",
					//"float-divide-by-zero",
					//"nonnull-attribute",
					//"null",
					//"shift-base",
					//"signed-integer-overflow",
					// TODO(danalbert): Fix UB in libc++'s __tree so we can turn this on.
					// https://llvm.org/PR19302
					// http://reviews.llvm.org/D6974
					// "object-size",
				)
			}
			sanitizers = append(sanitizers, c.sanitize.Properties.Sanitize.Misc_undefined...)
		}

		if Bool(c.sanitize.Properties.Sanitize.Diag.Undefined) {
			diagSanitizers = append(diagSanitizers, "undefined")
		}

		diagSanitizers = append(diagSanitizers, c.sanitize.Properties.Sanitize.Diag.Misc_undefined...)

		if Bool(c.sanitize.Properties.Sanitize.Address) {
			sanitizers = append(sanitizers, "address")
			diagSanitizers = append(diagSanitizers, "address")
		}

		if Bool(c.sanitize.Properties.Sanitize.Hwaddress) {
			sanitizers = append(sanitizers, "hwaddress")
		}

		if Bool(c.sanitize.Properties.Sanitize.Thread) {
			sanitizers = append(sanitizers, "thread")
		}

		if Bool(c.sanitize.Properties.Sanitize.Safestack) {
			sanitizers = append(sanitizers, "safe-stack")
		}

		if Bool(c.sanitize.Properties.Sanitize.Cfi) {
			sanitizers = append(sanitizers, "cfi")

			if Bool(c.sanitize.Properties.Sanitize.Diag.Cfi) {
				diagSanitizers = append(diagSanitizers, "cfi")
			}
		}

		if Bool(c.sanitize.Properties.Sanitize.Integer_overflow) {
			sanitizers = append(sanitizers, "unsigned-integer-overflow")
			sanitizers = append(sanitizers, "signed-integer-overflow")
			if Bool(c.sanitize.Properties.Sanitize.Diag.Integer_overflow) {
				diagSanitizers = append(diagSanitizers, "unsigned-integer-overflow")
				diagSanitizers = append(diagSanitizers, "signed-integer-overflow")
			}
		}

		if Bool(c.sanitize.Properties.Sanitize.Scudo) {
			sanitizers = append(sanitizers, "scudo")
		}

		if Bool(c.sanitize.Properties.Sanitize.Scs) {
			sanitizers = append(sanitizers, "shadow-call-stack")
		}

		// TODO(b/249094918) re-enable after clang version brought back in-line with upstream
		/*
			if Bool(c.sanitize.Properties.Sanitize.Memtag_heap) && c.Binary() {
				sanitizers = append(sanitizers, "memtag-heap")
			}

			if Bool(c.sanitize.Properties.Sanitize.Memtag_stack) {
				sanitizers = append(sanitizers, "memtag-stack")
			}
		*/

		if Bool(c.sanitize.Properties.Sanitize.Fuzzer) {
			sanitizers = append(sanitizers, "fuzzer-no-link")
		}

		// Save the list of sanitizers. These will be used again when generating
		// the build rules (for Cflags, etc.)
		c.sanitize.Properties.Sanitizers = sanitizers
		c.sanitize.Properties.DiagSanitizers = diagSanitizers

		// TODO(b/150822854) Hosts have a different default behavior and assume the runtime library is used.
		if c.Host() {
			diagSanitizers = sanitizers
		}

		// Determine the runtime library required
		runtimeLibrary := ""
		var extraStaticDeps []string
		toolchain := c.toolchain(mctx)
		if Bool(c.sanitize.Properties.Sanitize.Address) {
			runtimeLibrary = config.AddressSanitizerRuntimeLibrary(toolchain)
		} else if Bool(c.sanitize.Properties.Sanitize.Hwaddress) {
			if c.staticBinary() {
				runtimeLibrary = config.HWAddressSanitizerStaticLibrary(toolchain)
				extraStaticDeps = []string{"libdl"}
			} else {
				runtimeLibrary = config.HWAddressSanitizerRuntimeLibrary(toolchain)
			}
		} else if Bool(c.sanitize.Properties.Sanitize.Thread) {
			runtimeLibrary = config.ThreadSanitizerRuntimeLibrary(toolchain)
		} else if Bool(c.sanitize.Properties.Sanitize.Scudo) {
			if len(diagSanitizers) == 0 && !c.sanitize.Properties.UbsanRuntimeDep {
				runtimeLibrary = config.ScudoMinimalRuntimeLibrary(toolchain)
			} else {
				runtimeLibrary = config.ScudoRuntimeLibrary(toolchain)
			}
		} else if len(diagSanitizers) > 0 || c.sanitize.Properties.UbsanRuntimeDep ||
			Bool(c.sanitize.Properties.Sanitize.Fuzzer) ||
			Bool(c.sanitize.Properties.Sanitize.Undefined) ||
			Bool(c.sanitize.Properties.Sanitize.All_undefined) {
			runtimeLibrary = config.UndefinedBehaviorSanitizerRuntimeLibrary(toolchain)
			if c.staticBinary() {
				runtimeLibrary += ".static"
			}
		}

		addStaticDeps := func(deps ...string) {
			// If we're using snapshots, redirect to snapshot whenever possible
			snapshot := mctx.Provider(SnapshotInfoProvider).(SnapshotInfo)
			for idx, dep := range deps {
				if lib, ok := snapshot.StaticLibs[dep]; ok {
					deps[idx] = lib
				}
			}

			// static executable gets static runtime libs
			depTag := libraryDependencyTag{Kind: staticLibraryDependency, unexportedSymbols: true}
			variations := append(mctx.Target().Variations(),
				blueprint.Variation{Mutator: "link", Variation: "static"})
			if c.Device() {
				variations = append(variations, c.ImageVariation())
			}
			if c.UseSdk() {
				variations = append(variations,
					blueprint.Variation{Mutator: "sdk", Variation: "sdk"})
			}
			mctx.AddFarVariationDependencies(variations, depTag, deps...)

		}
		if enableMinimalRuntime(c.sanitize) || c.sanitize.Properties.MinimalRuntimeDep {
			addStaticDeps(config.UndefinedBehaviorSanitizerMinimalRuntimeLibrary(toolchain))
		}
		if c.sanitize.Properties.BuiltinsDep {
			addStaticDeps(config.BuiltinsRuntimeLibrary(toolchain))
		}

		if runtimeLibrary != "" && (toolchain.Bionic() || toolchain.Musl() || c.sanitize.Properties.UbsanRuntimeDep) {
			// UBSan is supported on non-bionic linux host builds as well

			// Adding dependency to the runtime library. We are using *FarVariation*
			// because the runtime libraries themselves are not mutated by sanitizer
			// mutators and thus don't have sanitizer variants whereas this module
			// has been already mutated.
			//
			// Note that by adding dependency with {static|shared}DepTag, the lib is
			// added to libFlags and LOCAL_SHARED_LIBRARIES by cc.Module
			if c.staticBinary() {
				addStaticDeps(runtimeLibrary)
				addStaticDeps(extraStaticDeps...)
			} else if !c.static() && !c.Header() {
				// If we're using snapshots, redirect to snapshot whenever possible
				snapshot := mctx.Provider(SnapshotInfoProvider).(SnapshotInfo)
				if lib, ok := snapshot.SharedLibs[runtimeLibrary]; ok {
					runtimeLibrary = lib
				}

				// Skip apex dependency check for sharedLibraryDependency
				// when sanitizer diags are enabled. Skipping the check will allow
				// building with diag libraries without having to list the
				// dependency in Apex's allowed_deps file.
				diagEnabled := len(diagSanitizers) > 0
				// dynamic executable and shared libs get shared runtime libs
				depTag := libraryDependencyTag{
					Kind:  sharedLibraryDependency,
					Order: earlyLibraryDependency,

					skipApexAllowedDependenciesCheck: diagEnabled,
				}
				variations := append(mctx.Target().Variations(),
					blueprint.Variation{Mutator: "link", Variation: "shared"})
				if c.Device() {
					variations = append(variations, c.ImageVariation())
				}
				if c.UseSdk() {
					variations = append(variations,
						blueprint.Variation{Mutator: "sdk", Variation: "sdk"})
				}
				AddSharedLibDependenciesWithVersions(mctx, c, variations, depTag, runtimeLibrary, "", true)
			}
			// static lib does not have dependency to the runtime library. The
			// dependency will be added to the executables or shared libs using
			// the static lib.
		}
	}
}

type Sanitizeable interface {
	android.Module
	IsSanitizerEnabled(config android.Config, sanitizerName string) bool
	EnableSanitizer(sanitizerName string)
	AddSanitizerDependencies(ctx android.BottomUpMutatorContext, sanitizerName string)
}

type JniSanitizeable interface {
	android.Module
	IsSanitizerEnabledForJni(ctx android.BaseModuleContext, sanitizerName string) bool
}

func (c *Module) MinimalRuntimeDep() bool {
	return c.sanitize.Properties.MinimalRuntimeDep
}

func (c *Module) UbsanRuntimeDep() bool {
	return c.sanitize.Properties.UbsanRuntimeDep
}

func (c *Module) SanitizePropDefined() bool {
	return c.sanitize != nil
}

func (c *Module) IsSanitizerEnabled(t SanitizerType) bool {
	return c.sanitize.isSanitizerEnabled(t)
}

func (c *Module) StaticallyLinked() bool {
	return c.static()
}

func (c *Module) SetInSanitizerDir() {
	if c.sanitize != nil {
		c.sanitize.Properties.InSanitizerDir = true
	}
}

func (c *Module) SetSanitizer(t SanitizerType, b bool) {
	if c.sanitize != nil {
		c.sanitize.SetSanitizer(t, b)
	}
}

var _ PlatformSanitizeable = (*Module)(nil)

type sanitizerStaticLibsMap struct {
	// libsMap contains one list of modules per each image and each arch.
	// e.g. libs[vendor]["arm"] contains arm modules installed to vendor
	libsMap       map[ImageVariantType]map[string][]string
	libsMapLock   sync.Mutex
	sanitizerType SanitizerType
}

func newSanitizerStaticLibsMap(t SanitizerType) *sanitizerStaticLibsMap {
	return &sanitizerStaticLibsMap{
		sanitizerType: t,
		libsMap:       make(map[ImageVariantType]map[string][]string),
	}
}

// Add the current module to sanitizer static libs maps
// Each module should pass its exported name as names of Make and Soong can differ.
func (s *sanitizerStaticLibsMap) add(c LinkableInterface, name string) {
	image := GetImageVariantType(c)
	arch := c.Module().Target().Arch.ArchType.String()

	s.libsMapLock.Lock()
	defer s.libsMapLock.Unlock()

	if _, ok := s.libsMap[image]; !ok {
		s.libsMap[image] = make(map[string][]string)
	}

	s.libsMap[image][arch] = append(s.libsMap[image][arch], name)
}

// Exports makefile variables in the following format:
// SOONG_{sanitizer}_{image}_{arch}_STATIC_LIBRARIES
// e.g. SOONG_cfi_core_x86_STATIC_LIBRARIES
// These are to be used by use_soong_sanitized_static_libraries.
// See build/make/core/binary.mk for more details.
func (s *sanitizerStaticLibsMap) exportToMake(ctx android.MakeVarsContext) {
	for _, image := range android.SortedStringKeys(s.libsMap) {
		archMap := s.libsMap[ImageVariantType(image)]
		for _, arch := range android.SortedStringKeys(archMap) {
			libs := archMap[arch]
			sort.Strings(libs)

			key := fmt.Sprintf(
				"SOONG_%s_%s_%s_STATIC_LIBRARIES",
				s.sanitizerType.variationName(),
				image, // already upper
				arch)

			ctx.Strict(key, strings.Join(libs, " "))
		}
	}
}

var cfiStaticLibsKey = android.NewOnceKey("cfiStaticLibs")

func cfiStaticLibs(config android.Config) *sanitizerStaticLibsMap {
	return config.Once(cfiStaticLibsKey, func() interface{} {
		return newSanitizerStaticLibsMap(cfi)
	}).(*sanitizerStaticLibsMap)
}

var hwasanStaticLibsKey = android.NewOnceKey("hwasanStaticLibs")

func hwasanStaticLibs(config android.Config) *sanitizerStaticLibsMap {
	return config.Once(hwasanStaticLibsKey, func() interface{} {
		return newSanitizerStaticLibsMap(Hwasan)
	}).(*sanitizerStaticLibsMap)
}

func enableMinimalRuntime(sanitize *sanitize) bool {
	if !Bool(sanitize.Properties.Sanitize.Address) &&
		!Bool(sanitize.Properties.Sanitize.Hwaddress) &&
		!Bool(sanitize.Properties.Sanitize.Fuzzer) &&
		(Bool(sanitize.Properties.Sanitize.Integer_overflow) ||
			len(sanitize.Properties.Sanitize.Misc_undefined) > 0 ||
			Bool(sanitize.Properties.Sanitize.Undefined) ||
			Bool(sanitize.Properties.Sanitize.All_undefined)) &&
		!(Bool(sanitize.Properties.Sanitize.Diag.Integer_overflow) ||
			Bool(sanitize.Properties.Sanitize.Diag.Cfi) ||
			Bool(sanitize.Properties.Sanitize.Diag.Undefined) ||
			len(sanitize.Properties.Sanitize.Diag.Misc_undefined) > 0) {

		return true
	}
	return false
}

func (m *Module) UbsanRuntimeNeeded() bool {
	return enableUbsanRuntime(m.sanitize)
}

func (m *Module) MinimalRuntimeNeeded() bool {
	return enableMinimalRuntime(m.sanitize)
}

func enableUbsanRuntime(sanitize *sanitize) bool {
	return Bool(sanitize.Properties.Sanitize.Diag.Integer_overflow) ||
		Bool(sanitize.Properties.Sanitize.Diag.Undefined) ||
		len(sanitize.Properties.Sanitize.Diag.Misc_undefined) > 0
}

func cfiMakeVarsProvider(ctx android.MakeVarsContext) {
	cfiStaticLibs(ctx.Config()).exportToMake(ctx)
}

func hwasanMakeVarsProvider(ctx android.MakeVarsContext) {
	hwasanStaticLibs(ctx.Config()).exportToMake(ctx)
}
