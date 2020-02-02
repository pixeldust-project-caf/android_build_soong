// Copyright 2018 Google Inc. All rights reserved.
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

package dexpreopt

import (
	"android/soong/android"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func testSystemModuleConfig(ctx android.PathContext, name string) ModuleConfig {
	return testModuleConfig(ctx, name, "system")
}

func testSystemProductModuleConfig(ctx android.PathContext, name string) ModuleConfig {
	return testModuleConfig(ctx, name, "system/product")
}

func testProductModuleConfig(ctx android.PathContext, name string) ModuleConfig {
	return testModuleConfig(ctx, name, "product")
}

func testModuleConfig(ctx android.PathContext, name, partition string) ModuleConfig {
	return ModuleConfig{
		Name:                            name,
		DexLocation:                     fmt.Sprintf("/%s/app/test/%s.apk", partition, name),
		BuildPath:                       android.PathForOutput(ctx, fmt.Sprintf("%s/%s.apk", name, name)),
		DexPath:                         android.PathForOutput(ctx, fmt.Sprintf("%s/dex/%s.jar", name, name)),
		UncompressedDex:                 false,
		HasApkLibraries:                 false,
		PreoptFlags:                     nil,
		ProfileClassListing:             android.OptionalPath{},
		ProfileIsTextListing:            false,
		EnforceUsesLibraries:            false,
		OptionalUsesLibraries:           nil,
		UsesLibraries:                   nil,
		LibraryPaths:                    nil,
		Archs:                           []android.ArchType{android.Arm},
		DexPreoptImages:                 android.Paths{android.PathForTesting("system/framework/arm/boot.art")},
		PreoptBootClassPathDexFiles:     nil,
		PreoptBootClassPathDexLocations: nil,
		PreoptExtractedApk:              false,
		NoCreateAppImage:                false,
		ForceCreateAppImage:             false,
		PresignedPrebuilt:               false,
		NoStripping:                     false,
		StripInputPath:                  android.PathForOutput(ctx, fmt.Sprintf("unstripped/%s.apk", name)),
		StripOutputPath:                 android.PathForOutput(ctx, fmt.Sprintf("stripped/%s.apk", name)),
	}
}

func TestDexPreopt(t *testing.T) {
	ctx := android.PathContextForTesting(android.TestConfig("out", nil), nil)
	global, module := GlobalConfigForTests(ctx), testSystemModuleConfig(ctx, "test")

	rule, err := GenerateDexpreoptRule(ctx, global, module)
	if err != nil {
		t.Fatal(err)
	}

	wantInstalls := android.RuleBuilderInstalls{
		{android.PathForOutput(ctx, "test/oat/arm/package.odex"), "/system/app/test/oat/arm/test.odex"},
		{android.PathForOutput(ctx, "test/oat/arm/package.vdex"), "/system/app/test/oat/arm/test.vdex"},
	}

	if rule.Installs().String() != wantInstalls.String() {
		t.Errorf("\nwant installs:\n   %v\ngot:\n   %v", wantInstalls, rule.Installs())
	}
}

func TestDexPreoptStrip(t *testing.T) {
	// Test that we panic if we strip in a configuration where stripping is not allowed.
	ctx := android.PathContextForTesting(android.TestConfig("out", nil), nil)
	global, module := GlobalConfigForTests(ctx), testSystemModuleConfig(ctx, "test")

	global.NeverAllowStripping = true
	module.NoStripping = false

	_, err := GenerateStripRule(global, module)
	if err == nil {
		t.Errorf("Expected an error when calling GenerateStripRule on a stripped module")
	}
}

func TestDexPreoptSystemOther(t *testing.T) {
	ctx := android.PathContextForTesting(android.TestConfig("out", nil), nil)
	global := GlobalConfigForTests(ctx)
	systemModule := testSystemModuleConfig(ctx, "Stest")
	systemProductModule := testSystemProductModuleConfig(ctx, "SPtest")
	productModule := testProductModuleConfig(ctx, "Ptest")

	global.HasSystemOther = true

	type moduleTest struct {
		module            ModuleConfig
		expectedPartition string
	}
	tests := []struct {
		patterns    []string
		moduleTests []moduleTest
	}{
		{
			patterns: []string{"app/%"},
			moduleTests: []moduleTest{
				{module: systemModule, expectedPartition: "system_other/system"},
				{module: systemProductModule, expectedPartition: "system/product"},
				{module: productModule, expectedPartition: "product"},
			},
		},
		{
			patterns: []string{"app/%", "product/app/%"},
			moduleTests: []moduleTest{
				{module: systemModule, expectedPartition: "system_other/system"},
				{module: systemProductModule, expectedPartition: "system_other/system/product"},
				{module: productModule, expectedPartition: "system_other/product"},
			},
		},
	}

	for _, test := range tests {
		global.PatternsOnSystemOther = test.patterns
		for _, mt := range test.moduleTests {
			rule, err := GenerateDexpreoptRule(ctx, global, mt.module)
			if err != nil {
				t.Fatal(err)
			}

			name := mt.module.Name
			wantInstalls := android.RuleBuilderInstalls{
				{android.PathForOutput(ctx, name+"/oat/arm/package.odex"), fmt.Sprintf("/%s/app/test/oat/arm/%s.odex", mt.expectedPartition, name)},
				{android.PathForOutput(ctx, name+"/oat/arm/package.vdex"), fmt.Sprintf("/%s/app/test/oat/arm/%s.vdex", mt.expectedPartition, name)},
			}

			if rule.Installs().String() != wantInstalls.String() {
				t.Errorf("\npatterns: %v\nwant installs:\n   %v\ngot:\n   %v", test.patterns, wantInstalls, rule.Installs())
			}
		}
	}

}

func TestDexPreoptProfile(t *testing.T) {
	ctx := android.PathContextForTesting(android.TestConfig("out", nil), nil)
	global, module := GlobalConfigForTests(ctx), testSystemModuleConfig(ctx, "test")

	module.ProfileClassListing = android.OptionalPathForPath(android.PathForTesting("profile"))

	rule, err := GenerateDexpreoptRule(ctx, global, module)
	if err != nil {
		t.Fatal(err)
	}

	wantInstalls := android.RuleBuilderInstalls{
		{android.PathForOutput(ctx, "test/profile.prof"), "/system/app/test/test.apk.prof"},
		{android.PathForOutput(ctx, "test/oat/arm/package.art"), "/system/app/test/oat/arm/test.art"},
		{android.PathForOutput(ctx, "test/oat/arm/package.odex"), "/system/app/test/oat/arm/test.odex"},
		{android.PathForOutput(ctx, "test/oat/arm/package.vdex"), "/system/app/test/oat/arm/test.vdex"},
	}

	if rule.Installs().String() != wantInstalls.String() {
		t.Errorf("\nwant installs:\n   %v\ngot:\n   %v", wantInstalls, rule.Installs())
	}
}

func TestStripDex(t *testing.T) {
	tests := []struct {
		name  string
		setup func(global *GlobalConfig, module *ModuleConfig)
		strip bool
	}{
		{
			name:  "default strip",
			setup: func(global *GlobalConfig, module *ModuleConfig) {},
			strip: true,
		},
		{
			name:  "global no stripping",
			setup: func(global *GlobalConfig, module *ModuleConfig) { global.DefaultNoStripping = true },
			strip: false,
		},
		{
			name:  "module no stripping",
			setup: func(global *GlobalConfig, module *ModuleConfig) { module.NoStripping = true },
			strip: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {

			ctx := android.PathContextForTesting(android.TestConfig("out", nil), nil)
			global, module := GlobalConfigForTests(ctx), testSystemModuleConfig(ctx, "test")

			test.setup(&global, &module)

			rule, err := GenerateStripRule(global, module)
			if err != nil {
				t.Fatal(err)
			}

			if test.strip {
				want := `zip2zip -i out/unstripped/test.apk -o out/stripped/test.apk -x "classes*.dex"`
				if len(rule.Commands()) < 1 || !strings.Contains(rule.Commands()[0], want) {
					t.Errorf("\nwant commands[0] to have:\n   %v\ngot:\n   %v", want, rule.Commands()[0])
				}
			} else {
				wantCommands := []string{
					"cp -f out/unstripped/test.apk out/stripped/test.apk",
				}
				if !reflect.DeepEqual(rule.Commands(), wantCommands) {
					t.Errorf("\nwant commands:\n   %v\ngot:\n   %v", wantCommands, rule.Commands())
				}
			}
		})
	}
}
