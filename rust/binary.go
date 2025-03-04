// Copyright 2019 The Android Open Source Project
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

package rust

import (
	"android/soong/android"
	"android/soong/bazel"
	"fmt"
)

func init() {
	android.RegisterModuleType("rust_binary", RustBinaryFactory)
	android.RegisterModuleType("rust_binary_host", RustBinaryHostFactory)
}

type BinaryCompilerProperties struct {
	// Builds this binary as a static binary. Implies prefer_rlib true.
	//
	// Static executables currently only support for bionic targets. Non-bionic targets will not produce a fully static
	// binary, but will still implicitly imply prefer_rlib true.
	Static_executable *bool `android:"arch_variant"`
}

type binaryInterface interface {
	binary() bool
	staticallyLinked() bool
	testBinary() bool
}

type binaryDecorator struct {
	*baseCompiler
	stripper Stripper

	Properties BinaryCompilerProperties
}

var _ compiler = (*binaryDecorator)(nil)

// rust_binary produces a binary that is runnable on a device.
func RustBinaryFactory() android.Module {
	module, _ := NewRustBinary(android.HostAndDeviceSupported)
	return module.Init()
}

func RustBinaryHostFactory() android.Module {
	module, _ := NewRustBinary(android.HostSupported)
	return module.Init()
}

func NewRustBinary(hod android.HostOrDeviceSupported) (*Module, *binaryDecorator) {
	module := newModule(hod, android.MultilibFirst)

	android.InitBazelModule(module)

	binary := &binaryDecorator{
		baseCompiler: NewBaseCompiler("bin", "", InstallInSystem),
	}

	module.compiler = binary

	return module, binary
}

func (binary *binaryDecorator) compilerFlags(ctx ModuleContext, flags Flags) Flags {
	flags = binary.baseCompiler.compilerFlags(ctx, flags)

	if ctx.Os().Linux() {
		flags.LinkFlags = append(flags.LinkFlags, "-Wl,--gc-sections")
	}

	if ctx.toolchain().Bionic() {
		// no-undefined-version breaks dylib compilation since __rust_*alloc* functions aren't defined,
		// but we can apply this to binaries.
		flags.LinkFlags = append(flags.LinkFlags,
			"-Wl,-z,nocopyreloc",
			"-Wl,--no-undefined-version")

		if Bool(binary.Properties.Static_executable) {
			flags.LinkFlags = append(flags.LinkFlags, "-static")
			flags.RustFlags = append(flags.RustFlags, "-C relocation-model=static")
		}
	}

	return flags
}

func (binary *binaryDecorator) compilerDeps(ctx DepsContext, deps Deps) Deps {
	deps = binary.baseCompiler.compilerDeps(ctx, deps)

	static := Bool(binary.Properties.Static_executable)
	if ctx.toolchain().Bionic() {
		deps = bionicDeps(ctx, deps, static)
		if static {
			deps.CrtBegin = []string{"crtbegin_static"}
		} else {
			deps.CrtBegin = []string{"crtbegin_dynamic"}
		}
		deps.CrtEnd = []string{"crtend_android"}
	} else if ctx.Os() == android.LinuxMusl {
		deps = muslDeps(ctx, deps, static)
		if static {
			deps.CrtBegin = []string{"libc_musl_crtbegin_static"}
		} else {
			deps.CrtBegin = []string{"libc_musl_crtbegin_dynamic"}
		}
		deps.CrtEnd = []string{"libc_musl_crtend"}
	}

	return deps
}

func (binary *binaryDecorator) compilerProps() []interface{} {
	return append(binary.baseCompiler.compilerProps(),
		&binary.Properties,
		&binary.stripper.StripProperties)
}

func (binary *binaryDecorator) nativeCoverage() bool {
	return true
}

func (binary *binaryDecorator) preferRlib() bool {
	return Bool(binary.baseCompiler.Properties.Prefer_rlib) || Bool(binary.Properties.Static_executable)
}

func (binary *binaryDecorator) compile(ctx ModuleContext, flags Flags, deps PathDeps) buildOutput {
	fileName := binary.getStem(ctx) + ctx.toolchain().ExecutableSuffix()
	outputFile := android.PathForModuleOut(ctx, fileName)
	ret := buildOutput{outputFile: outputFile}
	var crateRootPath android.Path
	if binary.baseCompiler.Properties.Crate_root == nil {
		crateRootPath, _ = srcPathFromModuleSrcs(ctx, binary.baseCompiler.Properties.Srcs)
	} else {
		crateRootPath = android.PathForModuleSrc(ctx, *binary.baseCompiler.Properties.Crate_root)
	}

	flags.RustFlags = append(flags.RustFlags, deps.depFlags...)
	flags.LinkFlags = append(flags.LinkFlags, deps.depLinkFlags...)
	flags.LinkFlags = append(flags.LinkFlags, deps.linkObjects.Strings()...)

	if binary.stripper.NeedsStrip(ctx) {
		strippedOutputFile := outputFile
		outputFile = android.PathForModuleOut(ctx, "unstripped", fileName)
		binary.stripper.StripExecutableOrSharedLib(ctx, outputFile, strippedOutputFile)

		binary.baseCompiler.strippedOutputFile = android.OptionalPathForPath(strippedOutputFile)
	}
	binary.baseCompiler.unstrippedOutputFile = outputFile

	ret.kytheFile = TransformSrcToBinary(ctx, crateRootPath, deps, flags, outputFile).kytheFile
	return ret
}

func (binary *binaryDecorator) autoDep(ctx android.BottomUpMutatorContext) autoDep {
	// Binaries default to dylib dependencies for device, rlib for host.
	if binary.preferRlib() {
		return rlibAutoDep
	} else if ctx.Device() {
		return dylibAutoDep
	} else {
		return rlibAutoDep
	}
}

func (binary *binaryDecorator) stdLinkage(ctx *depsContext) RustLinkage {
	if binary.preferRlib() {
		return RlibLinkage
	}
	return binary.baseCompiler.stdLinkage(ctx)
}

func (binary *binaryDecorator) binary() bool {
	return true
}

func (binary *binaryDecorator) staticallyLinked() bool {
	return Bool(binary.Properties.Static_executable)
}

func (binary *binaryDecorator) testBinary() bool {
	return false
}

type rustBinaryLibraryAttributes struct {
	Srcs            bazel.LabelListAttribute
	Compile_data    bazel.LabelListAttribute
	Crate_name      bazel.StringAttribute
	Edition         bazel.StringAttribute
	Crate_features  bazel.StringListAttribute
	Deps            bazel.LabelListAttribute
	Proc_macro_deps bazel.LabelListAttribute
	Rustc_flags     bazel.StringListAttribute
}

func binaryBp2build(ctx android.TopDownMutatorContext, m *Module) {
	binary := m.compiler.(*binaryDecorator)

	var srcs bazel.LabelList
	var compileData bazel.LabelList

	if binary.baseCompiler.Properties.Srcs[0] == "src/main.rs" {
		srcs = android.BazelLabelForModuleSrc(ctx, []string{"src/**/*.rs"})
		compileData = android.BazelLabelForModuleSrc(
			ctx,
			[]string{
				"src/**/*.proto",
				"examples/**/*.rs",
				"**/*.md",
				"templates/**/*.template",
			},
		)
	} else {
		srcs = android.BazelLabelForModuleSrc(ctx, binary.baseCompiler.Properties.Srcs)
	}

	deps := android.BazelLabelForModuleDeps(ctx, append(
		binary.baseCompiler.Properties.Rustlibs,
	))

	procMacroDeps := android.BazelLabelForModuleDeps(ctx, binary.baseCompiler.Properties.Proc_macros)

	var rustcFLags []string
	for _, cfg := range binary.baseCompiler.Properties.Cfgs {
		rustcFLags = append(rustcFLags, fmt.Sprintf("--cfg=%s", cfg))
	}

	attrs := &rustBinaryLibraryAttributes{
		Srcs: bazel.MakeLabelListAttribute(
			srcs,
		),
		Compile_data: bazel.MakeLabelListAttribute(
			compileData,
		),
		Crate_name: bazel.StringAttribute{
			Value: &binary.baseCompiler.Properties.Crate_name,
		},
		Edition: bazel.StringAttribute{
			Value: binary.baseCompiler.Properties.Edition,
		},
		Crate_features: bazel.StringListAttribute{
			Value: binary.baseCompiler.Properties.Features,
		},
		Deps: bazel.MakeLabelListAttribute(
			deps,
		),
		Proc_macro_deps: bazel.MakeLabelListAttribute(
			procMacroDeps,
		),
		Rustc_flags: bazel.StringListAttribute{
			Value: append(
				rustcFLags,
				binary.baseCompiler.Properties.Flags...,
			),
		},
	}

	ctx.CreateBazelTargetModule(
		bazel.BazelTargetModuleProperties{
			Rule_class:        "rust_binary",
			Bzl_load_location: "@rules_rust//rust:defs.bzl",
		},
		android.CommonAttributes{
			Name: m.Name(),
		},
		attrs,
	)
}
