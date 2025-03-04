// Copyright (C) 2018 The Android Open Source Project
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

// package apex implements build rules for creating the APEX files which are container for
// lower-level system components. See https://source.android.com/devices/tech/ota/apex
package apex

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"android/soong/bazel/cquery"

	"github.com/google/blueprint"
	"github.com/google/blueprint/bootstrap"
	"github.com/google/blueprint/proptools"

	"android/soong/android"
	"android/soong/bazel"
	"android/soong/bpf"
	"android/soong/cc"
	prebuilt_etc "android/soong/etc"
	"android/soong/filesystem"
	"android/soong/java"
	"android/soong/multitree"
	"android/soong/python"
	"android/soong/rust"
	"android/soong/sh"
)

func init() {
	registerApexBuildComponents(android.InitRegistrationContext)
}

func registerApexBuildComponents(ctx android.RegistrationContext) {
	ctx.RegisterModuleType("apex", BundleFactory)
	ctx.RegisterModuleType("apex_test", TestApexBundleFactory)
	ctx.RegisterModuleType("apex_vndk", vndkApexBundleFactory)
	ctx.RegisterModuleType("apex_defaults", DefaultsFactory)
	ctx.RegisterModuleType("prebuilt_apex", PrebuiltFactory)
	ctx.RegisterModuleType("override_apex", OverrideApexFactory)
	ctx.RegisterModuleType("apex_set", apexSetFactory)

	ctx.PreArchMutators(registerPreArchMutators)
	ctx.PreDepsMutators(RegisterPreDepsMutators)
	ctx.PostDepsMutators(RegisterPostDepsMutators)
}

func registerPreArchMutators(ctx android.RegisterMutatorsContext) {
	ctx.TopDown("prebuilt_apex_module_creator", prebuiltApexModuleCreatorMutator).Parallel()
}

func RegisterPreDepsMutators(ctx android.RegisterMutatorsContext) {
	ctx.TopDown("apex_vndk", apexVndkMutator).Parallel()
	ctx.BottomUp("apex_vndk_deps", apexVndkDepsMutator).Parallel()
}

func RegisterPostDepsMutators(ctx android.RegisterMutatorsContext) {
	ctx.TopDown("apex_info", apexInfoMutator).Parallel()
	ctx.BottomUp("apex_unique", apexUniqueVariationsMutator).Parallel()
	ctx.BottomUp("apex_test_for_deps", apexTestForDepsMutator).Parallel()
	ctx.BottomUp("apex_test_for", apexTestForMutator).Parallel()
	// Run mark_platform_availability before the apexMutator as the apexMutator needs to know whether
	// it should create a platform variant.
	ctx.BottomUp("mark_platform_availability", markPlatformAvailability).Parallel()
	ctx.BottomUp("apex", apexMutator).Parallel()
	ctx.BottomUp("apex_directly_in_any", apexDirectlyInAnyMutator).Parallel()
	ctx.BottomUp("apex_flattened", apexFlattenedMutator).Parallel()
	ctx.BottomUp("apex_dcla_deps", apexDCLADepsMutator).Parallel()
	// Register after apex_info mutator so that it can use ApexVariationName
	ctx.TopDown("apex_strict_updatability_lint", apexStrictUpdatibilityLintMutator).Parallel()
}

type apexBundleProperties struct {
	// Json manifest file describing meta info of this APEX bundle. Refer to
	// system/apex/proto/apex_manifest.proto for the schema. Default: "apex_manifest.json"
	Manifest *string `android:"path"`

	// AndroidManifest.xml file used for the zip container of this APEX bundle. If unspecified,
	// a default one is automatically generated.
	AndroidManifest *string `android:"path"`

	// Determines the file contexts file for setting the security contexts to files in this APEX
	// bundle. For platform APEXes, this should points to a file under /system/sepolicy Default:
	// /system/sepolicy/apex/<module_name>_file_contexts.
	File_contexts *string `android:"path"`

	// By default, file_contexts is amended by force-labelling / and /apex_manifest.pb as system_file
	// to avoid mistakes. When set as true, no force-labelling.
	Use_file_contexts_as_is *bool

	// Path to the canned fs config file for customizing file's
	// uid/gid/mod/capabilities. The content of this file is appended to the
	// default config, so that the custom entries are preferred. The format is
	// /<path_or_glob> <uid> <gid> <mode> [capabilities=0x<cap>], where
	// path_or_glob is a path or glob pattern for a file or set of files,
	// uid/gid are numerial values of user ID and group ID, mode is octal value
	// for the file mode, and cap is hexadecimal value for the capability.
	Canned_fs_config *string `android:"path"`

	ApexNativeDependencies

	Multilib apexMultilibProperties

	// List of runtime resource overlays (RROs) that are embedded inside this APEX.
	Rros []string

	// List of bootclasspath fragments that are embedded inside this APEX bundle.
	Bootclasspath_fragments []string

	// List of systemserverclasspath fragments that are embedded inside this APEX bundle.
	Systemserverclasspath_fragments []string

	// List of java libraries that are embedded inside this APEX bundle.
	Java_libs []string

	// List of sh binaries that are embedded inside this APEX bundle.
	Sh_binaries []string

	// List of platform_compat_config files that are embedded inside this APEX bundle.
	Compat_configs []string

	// List of filesystem images that are embedded inside this APEX bundle.
	Filesystems []string

	// The minimum SDK version that this APEX must support at minimum. This is usually set to
	// the SDK version that the APEX was first introduced.
	Min_sdk_version *string

	// Whether this APEX is considered updatable or not. When set to true, this will enforce
	// additional rules for making sure that the APEX is truly updatable. To be updatable,
	// min_sdk_version should be set as well. This will also disable the size optimizations like
	// symlinking to the system libs. Default is true.
	Updatable *bool

	// Marks that this APEX is designed to be updatable in the future, although it's not
	// updatable yet. This is used to mimic some of the build behaviors that are applied only to
	// updatable APEXes. Currently, this disables the size optimization, so that the size of
	// APEX will not increase when the APEX is actually marked as truly updatable. Default is
	// false.
	Future_updatable *bool

	// Whether this APEX can use platform APIs or not. Can be set to true only when `updatable:
	// false`. Default is false.
	Platform_apis *bool

	// Whether this APEX is installable to one of the partitions like system, vendor, etc.
	// Default: true.
	Installable *bool

	// If set true, VNDK libs are considered as stable libs and are not included in this APEX.
	// Should be only used in non-system apexes (e.g. vendor: true). Default is false.
	Use_vndk_as_stable *bool

	// The type of APEX to build. Controls what the APEX payload is. Either 'image', 'zip' or
	// 'both'. When set to image, contents are stored in a filesystem image inside a zip
	// container. When set to zip, contents are stored in a zip container directly. This type is
	// mostly for host-side debugging. When set to both, the two types are both built. Default
	// is 'image'.
	Payload_type *string

	// The type of filesystem to use when the payload_type is 'image'. Either 'ext4', 'f2fs'
	// or 'erofs'. Default 'ext4'.
	Payload_fs_type *string

	// For telling the APEX to ignore special handling for system libraries such as bionic.
	// Default is false.
	Ignore_system_library_special_case *bool

	// Whenever apex_payload.img of the APEX should include dm-verity hashtree.
	// Default value is true.
	Generate_hashtree *bool

	// Whenever apex_payload.img of the APEX should not be dm-verity signed. Should be only
	// used in tests.
	Test_only_unsigned_payload *bool

	// Whenever apex should be compressed, regardless of product flag used. Should be only
	// used in tests.
	Test_only_force_compression *bool

	// Put extra tags (signer=<value>) to apexkeys.txt, so that release tools can sign this apex
	// with the tool to sign payload contents.
	Custom_sign_tool *string

	// Whether this is a dynamic common lib apex, if so the native shared libs will be placed
	// in a special way that include the digest of the lib file under /lib(64)?
	Dynamic_common_lib_apex *bool

	// Canonical name of this APEX bundle. Used to determine the path to the
	// activated APEX on device (i.e. /apex/<apexVariationName>), and used for the
	// apex mutator variations. For override_apex modules, this is the name of the
	// overridden base module.
	ApexVariationName string `blueprint:"mutated"`

	IsCoverageVariant bool `blueprint:"mutated"`

	// List of sanitizer names that this APEX is enabled for
	SanitizerNames []string `blueprint:"mutated"`

	PreventInstall bool `blueprint:"mutated"`

	HideFromMake bool `blueprint:"mutated"`

	// Internal package method for this APEX. When payload_type is image, this can be either
	// imageApex or flattenedApex depending on Config.FlattenApex(). When payload_type is zip,
	// this becomes zipApex.
	ApexType apexPackaging `blueprint:"mutated"`
}

type ApexNativeDependencies struct {
	// List of native libraries that are embedded inside this APEX.
	Native_shared_libs []string

	// List of JNI libraries that are embedded inside this APEX.
	Jni_libs []string

	// List of rust dyn libraries that are embedded inside this APEX.
	Rust_dyn_libs []string

	// List of native executables that are embedded inside this APEX.
	Binaries []string

	// List of native tests that are embedded inside this APEX.
	Tests []string

	// List of filesystem images that are embedded inside this APEX bundle.
	Filesystems []string

	// List of native libraries to exclude from this APEX.
	Exclude_native_shared_libs []string

	// List of JNI libraries to exclude from this APEX.
	Exclude_jni_libs []string

	// List of rust dyn libraries to exclude from this APEX.
	Exclude_rust_dyn_libs []string

	// List of native executables to exclude from this APEX.
	Exclude_binaries []string

	// List of native tests to exclude from this APEX.
	Exclude_tests []string

	// List of filesystem images to exclude from this APEX bundle.
	Exclude_filesystems []string
}

// Merge combines another ApexNativeDependencies into this one
func (a *ApexNativeDependencies) Merge(b ApexNativeDependencies) {
	a.Native_shared_libs = append(a.Native_shared_libs, b.Native_shared_libs...)
	a.Jni_libs = append(a.Jni_libs, b.Jni_libs...)
	a.Rust_dyn_libs = append(a.Rust_dyn_libs, b.Rust_dyn_libs...)
	a.Binaries = append(a.Binaries, b.Binaries...)
	a.Tests = append(a.Tests, b.Tests...)
	a.Filesystems = append(a.Filesystems, b.Filesystems...)

	a.Exclude_native_shared_libs = append(a.Exclude_native_shared_libs, b.Exclude_native_shared_libs...)
	a.Exclude_jni_libs = append(a.Exclude_jni_libs, b.Exclude_jni_libs...)
	a.Exclude_rust_dyn_libs = append(a.Exclude_rust_dyn_libs, b.Exclude_rust_dyn_libs...)
	a.Exclude_binaries = append(a.Exclude_binaries, b.Exclude_binaries...)
	a.Exclude_tests = append(a.Exclude_tests, b.Exclude_tests...)
	a.Exclude_filesystems = append(a.Exclude_filesystems, b.Exclude_filesystems...)
}

type apexMultilibProperties struct {
	// Native dependencies whose compile_multilib is "first"
	First ApexNativeDependencies

	// Native dependencies whose compile_multilib is "both"
	Both ApexNativeDependencies

	// Native dependencies whose compile_multilib is "prefer32"
	Prefer32 ApexNativeDependencies

	// Native dependencies whose compile_multilib is "32"
	Lib32 ApexNativeDependencies

	// Native dependencies whose compile_multilib is "64"
	Lib64 ApexNativeDependencies
}

type apexTargetBundleProperties struct {
	Target struct {
		// Multilib properties only for android.
		Android struct {
			Multilib apexMultilibProperties
		}

		// Multilib properties only for host.
		Host struct {
			Multilib apexMultilibProperties
		}

		// Multilib properties only for host linux_bionic.
		Linux_bionic struct {
			Multilib apexMultilibProperties
		}

		// Multilib properties only for host linux_glibc.
		Linux_glibc struct {
			Multilib apexMultilibProperties
		}
	}
}

type apexArchBundleProperties struct {
	Arch struct {
		Arm struct {
			ApexNativeDependencies
		}
		Arm64 struct {
			ApexNativeDependencies
		}
		Riscv64 struct {
			ApexNativeDependencies
		}
		X86 struct {
			ApexNativeDependencies
		}
		X86_64 struct {
			ApexNativeDependencies
		}
	}
}

// These properties can be used in override_apex to override the corresponding properties in the
// base apex.
type overridableProperties struct {
	// List of APKs that are embedded inside this APEX.
	Apps []string

	// List of prebuilt files that are embedded inside this APEX bundle.
	Prebuilts []string

	// List of BPF programs inside this APEX bundle.
	Bpfs []string

	// Names of modules to be overridden. Listed modules can only be other binaries (in Make or
	// Soong). This does not completely prevent installation of the overridden binaries, but if
	// both binaries would be installed by default (in PRODUCT_PACKAGES) the other binary will
	// be removed from PRODUCT_PACKAGES.
	Overrides []string

	// Logging parent value.
	Logging_parent string

	// Apex Container package name. Override value for attribute package:name in
	// AndroidManifest.xml
	Package_name string

	// A txt file containing list of files that are allowed to be included in this APEX.
	Allowed_files *string `android:"path"`

	// Name of the apex_key module that provides the private key to sign this APEX bundle.
	Key *string

	// Specifies the certificate and the private key to sign the zip container of this APEX. If
	// this is "foo", foo.x509.pem and foo.pk8 under PRODUCT_DEFAULT_DEV_CERTIFICATE are used
	// as the certificate and the private key, respectively. If this is ":module", then the
	// certificate and the private key are provided from the android_app_certificate module
	// named "module".
	Certificate *string

	// Whether this APEX can be compressed or not. Setting this property to false means this
	// APEX will never be compressed. When set to true, APEX will be compressed if other
	// conditions, e.g., target device needs to support APEX compression, are also fulfilled.
	// Default: false.
	Compressible *bool

	// Trim against a specific Dynamic Common Lib APEX
	Trim_against *string
}

type apexBundle struct {
	// Inherited structs
	android.ModuleBase
	android.DefaultableModuleBase
	android.OverridableModuleBase
	android.BazelModuleBase
	multitree.ExportableModuleBase

	// Properties
	properties            apexBundleProperties
	targetProperties      apexTargetBundleProperties
	archProperties        apexArchBundleProperties
	overridableProperties overridableProperties
	vndkProperties        apexVndkProperties // only for apex_vndk modules

	///////////////////////////////////////////////////////////////////////////////////////////
	// Inputs

	// Keys for apex_paylaod.img
	publicKeyFile  android.Path
	privateKeyFile android.Path

	// Cert/priv-key for the zip container
	containerCertificateFile android.Path
	containerPrivateKeyFile  android.Path

	// Flags for special variants of APEX
	testApex bool
	vndkApex bool

	// Tells whether this variant of the APEX bundle is the primary one or not. Only the primary
	// one gets installed to the device.
	primaryApexType bool

	// Suffix of module name in Android.mk ".flattened", ".apex", ".zipapex", or ""
	suffix string

	// File system type of apex_payload.img
	payloadFsType fsType

	// Whether to create symlink to the system file instead of having a file inside the apex or
	// not
	linkToSystemLib bool

	// List of files to be included in this APEX. This is filled in the first part of
	// GenerateAndroidBuildActions.
	filesInfo []apexFile

	// List of other module names that should be installed when this APEX gets installed (LOCAL_REQUIRED_MODULES).
	makeModulesToInstall []string

	///////////////////////////////////////////////////////////////////////////////////////////
	// Outputs (final and intermediates)

	// Processed apex manifest in JSONson format (for Q)
	manifestJsonOut android.WritablePath

	// Processed apex manifest in PB format (for R+)
	manifestPbOut android.WritablePath

	// Processed file_contexts files
	fileContexts android.WritablePath

	// The built APEX file. This is the main product.
	// Could be .apex or .capex
	outputFile android.WritablePath

	// The built uncompressed .apex file.
	outputApexFile android.WritablePath

	// The built APEX file in app bundle format. This file is not directly installed to the
	// device. For an APEX, multiple app bundles are created each of which is for a specific ABI
	// like arm, arm64, x86, etc. Then they are processed again (outside of the Android build
	// system) to be merged into a single app bundle file that Play accepts. See
	// vendor/google/build/build_unbundled_mainline_module.sh for more detail.
	bundleModuleFile android.WritablePath

	// Target directory to install this APEX. Usually out/target/product/<device>/<partition>/apex.
	installDir android.InstallPath

	// Path where this APEX was installed.
	installedFile android.InstallPath

	// Installed locations of symlinks for backward compatibility.
	compatSymlinks android.InstallPaths

	// Text file having the list of individual files that are included in this APEX. Used for
	// debugging purpose.
	installedFilesFile android.WritablePath

	// List of module names that this APEX is including (to be shown via *-deps-info target).
	// Used for debugging purpose.
	android.ApexBundleDepsInfo

	// Optional list of lint report zip files for apexes that contain java or app modules
	lintReports android.Paths

	isCompressed bool

	// Path of API coverage generate file
	nativeApisUsedByModuleFile   android.ModuleOutPath
	nativeApisBackedByModuleFile android.ModuleOutPath
	javaApisUsedByModuleFile     android.ModuleOutPath

	// Collect the module directory for IDE info in java/jdeps.go.
	modulePaths []string
}

// apexFileClass represents a type of file that can be included in APEX.
type apexFileClass int

const (
	app apexFileClass = iota
	appSet
	etc
	goBinary
	javaSharedLib
	nativeExecutable
	nativeSharedLib
	nativeTest
	pyBinary
	shBinary
)

var (
	classes = map[string]apexFileClass{
		"app":              app,
		"appSet":           appSet,
		"etc":              etc,
		"goBinary":         goBinary,
		"javaSharedLib":    javaSharedLib,
		"nativeExecutable": nativeExecutable,
		"nativeSharedLib":  nativeSharedLib,
		"nativeTest":       nativeTest,
		"pyBinary":         pyBinary,
		"shBinary":         shBinary,
	}
)

// apexFile represents a file in an APEX bundle. This is created during the first half of
// GenerateAndroidBuildActions by traversing the dependencies of the APEX. Then in the second half
// of the function, this is used to create commands that copies the files into a staging directory,
// where they are packaged into the APEX file. This struct is also used for creating Make modules
// for each of the files in case when the APEX is flattened.
type apexFile struct {
	// buildFile is put in the installDir inside the APEX.
	builtFile  android.Path
	installDir string
	partition  string
	customStem string
	symlinks   []string // additional symlinks

	// Info for Android.mk Module name of `module` in AndroidMk. Note the generated AndroidMk
	// module for apexFile is named something like <AndroidMk module name>.<apex name>[<apex
	// suffix>]
	androidMkModuleName       string             // becomes LOCAL_MODULE
	class                     apexFileClass      // becomes LOCAL_MODULE_CLASS
	moduleDir                 string             // becomes LOCAL_PATH
	requiredModuleNames       []string           // becomes LOCAL_REQUIRED_MODULES
	targetRequiredModuleNames []string           // becomes LOCAL_TARGET_REQUIRED_MODULES
	hostRequiredModuleNames   []string           // becomes LOCAL_HOST_REQUIRED_MODULES
	dataPaths                 []android.DataPath // becomes LOCAL_TEST_DATA

	jacocoReportClassesFile android.Path     // only for javalibs and apps
	lintDepSets             java.LintDepSets // only for javalibs and apps
	certificate             java.Certificate // only for apps
	overriddenPackageName   string           // only for apps

	transitiveDep bool
	isJniLib      bool

	multilib string

	isBazelPrebuilt     bool
	unstrippedBuiltFile android.Path
	arch                string

	// TODO(jiyong): remove this
	module android.Module
}

// TODO(jiyong): shorten the arglist using an option struct
func newApexFile(ctx android.BaseModuleContext, builtFile android.Path, androidMkModuleName string, installDir string, class apexFileClass, module android.Module) apexFile {
	ret := apexFile{
		builtFile:           builtFile,
		installDir:          installDir,
		androidMkModuleName: androidMkModuleName,
		class:               class,
		module:              module,
	}
	if module != nil {
		ret.moduleDir = ctx.OtherModuleDir(module)
		ret.partition = module.PartitionTag(ctx.DeviceConfig())
		ret.requiredModuleNames = module.RequiredModuleNames()
		ret.targetRequiredModuleNames = module.TargetRequiredModuleNames()
		ret.hostRequiredModuleNames = module.HostRequiredModuleNames()
		ret.multilib = module.Target().Arch.ArchType.Multilib
	}
	return ret
}

func (af *apexFile) ok() bool {
	return af.builtFile != nil && af.builtFile.String() != ""
}

// apexRelativePath returns the relative path of the given path from the install directory of this
// apexFile.
// TODO(jiyong): rename this
func (af *apexFile) apexRelativePath(path string) string {
	return filepath.Join(af.installDir, path)
}

// path returns path of this apex file relative to the APEX root
func (af *apexFile) path() string {
	return af.apexRelativePath(af.stem())
}

// stem returns the base filename of this apex file
func (af *apexFile) stem() string {
	if af.customStem != "" {
		return af.customStem
	}
	return af.builtFile.Base()
}

// symlinkPaths returns paths of the symlinks (if any) relative to the APEX root
func (af *apexFile) symlinkPaths() []string {
	var ret []string
	for _, symlink := range af.symlinks {
		ret = append(ret, af.apexRelativePath(symlink))
	}
	return ret
}

// availableToPlatform tests whether this apexFile is from a module that can be installed to the
// platform.
func (af *apexFile) availableToPlatform() bool {
	if af.module == nil {
		return false
	}
	if am, ok := af.module.(android.ApexModule); ok {
		return am.AvailableFor(android.AvailableToPlatform)
	}
	return false
}

////////////////////////////////////////////////////////////////////////////////////////////////////
// Mutators
//
// Brief description about mutators for APEX. The following three mutators are the most important
// ones.
//
// 1) DepsMutator: from the properties like native_shared_libs, java_libs, etc., modules are added
// to the (direct) dependencies of this APEX bundle.
//
// 2) apexInfoMutator: this is a post-deps mutator, so runs after DepsMutator. Its goal is to
// collect modules that are direct and transitive dependencies of each APEX bundle. The collected
// modules are marked as being included in the APEX via BuildForApex().
//
// 3) apexMutator: this is a post-deps mutator that runs after apexInfoMutator. For each module that
// are marked by the apexInfoMutator, apex variations are created using CreateApexVariations().

type dependencyTag struct {
	blueprint.BaseDependencyTag
	name string

	// Determines if the dependent will be part of the APEX payload. Can be false for the
	// dependencies to the signing key module, etc.
	payload bool

	// True if the dependent can only be a source module, false if a prebuilt module is a suitable
	// replacement. This is needed because some prebuilt modules do not provide all the information
	// needed by the apex.
	sourceOnly bool

	// If not-nil and an APEX is a member of an SDK then dependencies of that APEX with this tag will
	// also be added as exported members of that SDK.
	memberType android.SdkMemberType
}

func (d *dependencyTag) SdkMemberType(_ android.Module) android.SdkMemberType {
	return d.memberType
}

func (d *dependencyTag) ExportMember() bool {
	return true
}

func (d *dependencyTag) String() string {
	return fmt.Sprintf("apex.dependencyTag{%q}", d.name)
}

func (d *dependencyTag) ReplaceSourceWithPrebuilt() bool {
	return !d.sourceOnly
}

var _ android.ReplaceSourceWithPrebuilt = &dependencyTag{}
var _ android.SdkMemberDependencyTag = &dependencyTag{}

var (
	androidAppTag   = &dependencyTag{name: "androidApp", payload: true}
	bpfTag          = &dependencyTag{name: "bpf", payload: true}
	certificateTag  = &dependencyTag{name: "certificate"}
	dclaTag         = &dependencyTag{name: "dcla"}
	executableTag   = &dependencyTag{name: "executable", payload: true}
	fsTag           = &dependencyTag{name: "filesystem", payload: true}
	bcpfTag         = &dependencyTag{name: "bootclasspathFragment", payload: true, sourceOnly: true, memberType: java.BootclasspathFragmentSdkMemberType}
	sscpfTag        = &dependencyTag{name: "systemserverclasspathFragment", payload: true, sourceOnly: true, memberType: java.SystemServerClasspathFragmentSdkMemberType}
	compatConfigTag = &dependencyTag{name: "compatConfig", payload: true, sourceOnly: true, memberType: java.CompatConfigSdkMemberType}
	javaLibTag      = &dependencyTag{name: "javaLib", payload: true}
	jniLibTag       = &dependencyTag{name: "jniLib", payload: true}
	keyTag          = &dependencyTag{name: "key"}
	prebuiltTag     = &dependencyTag{name: "prebuilt", payload: true}
	rroTag          = &dependencyTag{name: "rro", payload: true}
	sharedLibTag    = &dependencyTag{name: "sharedLib", payload: true}
	testForTag      = &dependencyTag{name: "test for"}
	testTag         = &dependencyTag{name: "test", payload: true}
	shBinaryTag     = &dependencyTag{name: "shBinary", payload: true}
)

// TODO(jiyong): shorten this function signature
func addDependenciesForNativeModules(ctx android.BottomUpMutatorContext, nativeModules ApexNativeDependencies, target android.Target, imageVariation string) {
	binVariations := target.Variations()
	libVariations := append(target.Variations(), blueprint.Variation{Mutator: "link", Variation: "shared"})
	rustLibVariations := append(target.Variations(), blueprint.Variation{Mutator: "rust_libraries", Variation: "dylib"})

	if ctx.Device() {
		binVariations = append(binVariations, blueprint.Variation{Mutator: "image", Variation: imageVariation})
		libVariations = append(libVariations, blueprint.Variation{Mutator: "image", Variation: imageVariation})
		rustLibVariations = append(rustLibVariations, blueprint.Variation{Mutator: "image", Variation: imageVariation})
	}

	// Use *FarVariation* to be able to depend on modules having conflicting variations with
	// this module. This is required since arch variant of an APEX bundle is 'common' but it is
	// 'arm' or 'arm64' for native shared libs.
	ctx.AddFarVariationDependencies(binVariations, executableTag,
		android.RemoveListFromList(nativeModules.Binaries, nativeModules.Exclude_binaries)...)
	ctx.AddFarVariationDependencies(binVariations, testTag,
		android.RemoveListFromList(nativeModules.Tests, nativeModules.Exclude_tests)...)
	ctx.AddFarVariationDependencies(libVariations, jniLibTag,
		android.RemoveListFromList(nativeModules.Jni_libs, nativeModules.Exclude_jni_libs)...)
	ctx.AddFarVariationDependencies(libVariations, sharedLibTag,
		android.RemoveListFromList(nativeModules.Native_shared_libs, nativeModules.Exclude_native_shared_libs)...)
	ctx.AddFarVariationDependencies(rustLibVariations, sharedLibTag,
		android.RemoveListFromList(nativeModules.Rust_dyn_libs, nativeModules.Exclude_rust_dyn_libs)...)
	ctx.AddFarVariationDependencies(target.Variations(), fsTag,
		android.RemoveListFromList(nativeModules.Filesystems, nativeModules.Exclude_filesystems)...)
}

func (a *apexBundle) combineProperties(ctx android.BottomUpMutatorContext) {
	if ctx.Device() {
		proptools.AppendProperties(&a.properties.Multilib, &a.targetProperties.Target.Android.Multilib, nil)
	} else {
		proptools.AppendProperties(&a.properties.Multilib, &a.targetProperties.Target.Host.Multilib, nil)
		if ctx.Os().Bionic() {
			proptools.AppendProperties(&a.properties.Multilib, &a.targetProperties.Target.Linux_bionic.Multilib, nil)
		} else {
			proptools.AppendProperties(&a.properties.Multilib, &a.targetProperties.Target.Linux_glibc.Multilib, nil)
		}
	}
}

// getImageVariationPair returns a pair for the image variation name as its
// prefix and suffix. The prefix indicates whether it's core/vendor/product and the
// suffix indicates the vndk version when it's vendor or product.
// getImageVariation can simply join the result of this function to get the
// image variation name.
func (a *apexBundle) getImageVariationPair(deviceConfig android.DeviceConfig) (string, string) {
	if a.vndkApex {
		return cc.VendorVariationPrefix, a.vndkVersion(deviceConfig)
	}

	var prefix string
	var vndkVersion string
	if deviceConfig.VndkVersion() != "" {
		if a.SocSpecific() || a.DeviceSpecific() {
			prefix = cc.VendorVariationPrefix
			vndkVersion = deviceConfig.VndkVersion()
		} else if a.ProductSpecific() {
			prefix = cc.ProductVariationPrefix
			vndkVersion = deviceConfig.PlatformVndkVersion()
		}
	}
	if vndkVersion == "current" {
		vndkVersion = deviceConfig.PlatformVndkVersion()
	}
	if vndkVersion != "" {
		return prefix, vndkVersion
	}

	return android.CoreVariation, "" // The usual case
}

// getImageVariation returns the image variant name for this apexBundle. In most cases, it's simply
// android.CoreVariation, but gets complicated for the vendor APEXes and the VNDK APEX.
func (a *apexBundle) getImageVariation(ctx android.BottomUpMutatorContext) string {
	prefix, vndkVersion := a.getImageVariationPair(ctx.DeviceConfig())
	return prefix + vndkVersion
}

func (a *apexBundle) DepsMutator(ctx android.BottomUpMutatorContext) {
	// apexBundle is a multi-arch targets module. Arch variant of apexBundle is set to 'common'.
	// arch-specific targets are enabled by the compile_multilib setting of the apex bundle. For
	// each target os/architectures, appropriate dependencies are selected by their
	// target.<os>.multilib.<type> groups and are added as (direct) dependencies.
	targets := ctx.MultiTargets()
	imageVariation := a.getImageVariation(ctx)

	a.combineProperties(ctx)

	has32BitTarget := false
	for _, target := range targets {
		if target.Arch.ArchType.Multilib == "lib32" {
			has32BitTarget = true
		}
	}
	for i, target := range targets {
		// Don't include artifacts for the host cross targets because there is no way for us
		// to run those artifacts natively on host
		if target.HostCross {
			continue
		}

		var deps ApexNativeDependencies

		// Add native modules targeting both ABIs. When multilib.* is omitted for
		// native_shared_libs/jni_libs/tests, it implies multilib.both
		deps.Merge(a.properties.Multilib.Both)
		deps.Merge(ApexNativeDependencies{
			Native_shared_libs: a.properties.Native_shared_libs,
			Tests:              a.properties.Tests,
			Jni_libs:           a.properties.Jni_libs,
			Binaries:           nil,
		})

		// Add native modules targeting the first ABI When multilib.* is omitted for
		// binaries, it implies multilib.first
		isPrimaryAbi := i == 0
		if isPrimaryAbi {
			deps.Merge(a.properties.Multilib.First)
			deps.Merge(ApexNativeDependencies{
				Native_shared_libs: nil,
				Tests:              nil,
				Jni_libs:           nil,
				Binaries:           a.properties.Binaries,
			})
		}

		// Add native modules targeting either 32-bit or 64-bit ABI
		switch target.Arch.ArchType.Multilib {
		case "lib32":
			deps.Merge(a.properties.Multilib.Lib32)
			deps.Merge(a.properties.Multilib.Prefer32)
		case "lib64":
			deps.Merge(a.properties.Multilib.Lib64)
			if !has32BitTarget {
				deps.Merge(a.properties.Multilib.Prefer32)
			}
		}

		// Add native modules targeting a specific arch variant
		switch target.Arch.ArchType {
		case android.Arm:
			deps.Merge(a.archProperties.Arch.Arm.ApexNativeDependencies)
		case android.Arm64:
			deps.Merge(a.archProperties.Arch.Arm64.ApexNativeDependencies)
		case android.Riscv64:
			deps.Merge(a.archProperties.Arch.Riscv64.ApexNativeDependencies)
		case android.X86:
			deps.Merge(a.archProperties.Arch.X86.ApexNativeDependencies)
		case android.X86_64:
			deps.Merge(a.archProperties.Arch.X86_64.ApexNativeDependencies)
		default:
			panic(fmt.Errorf("unsupported arch %v\n", ctx.Arch().ArchType))
		}

		addDependenciesForNativeModules(ctx, deps, target, imageVariation)
		ctx.AddFarVariationDependencies([]blueprint.Variation{
			{Mutator: "os", Variation: target.OsVariation()},
			{Mutator: "arch", Variation: target.ArchVariation()},
		}, shBinaryTag, a.properties.Sh_binaries...)
	}

	// Common-arch dependencies come next
	commonVariation := ctx.Config().AndroidCommonTarget.Variations()
	ctx.AddFarVariationDependencies(commonVariation, rroTag, a.properties.Rros...)
	ctx.AddFarVariationDependencies(commonVariation, bcpfTag, a.properties.Bootclasspath_fragments...)
	ctx.AddFarVariationDependencies(commonVariation, sscpfTag, a.properties.Systemserverclasspath_fragments...)
	ctx.AddFarVariationDependencies(commonVariation, javaLibTag, a.properties.Java_libs...)
	ctx.AddFarVariationDependencies(commonVariation, fsTag, a.properties.Filesystems...)
	ctx.AddFarVariationDependencies(commonVariation, compatConfigTag, a.properties.Compat_configs...)
}

// DepsMutator for the overridden properties.
func (a *apexBundle) OverridablePropertiesDepsMutator(ctx android.BottomUpMutatorContext) {
	if a.overridableProperties.Allowed_files != nil {
		android.ExtractSourceDeps(ctx, a.overridableProperties.Allowed_files)
	}

	commonVariation := ctx.Config().AndroidCommonTarget.Variations()
	ctx.AddFarVariationDependencies(commonVariation, androidAppTag, a.overridableProperties.Apps...)
	ctx.AddFarVariationDependencies(commonVariation, bpfTag, a.overridableProperties.Bpfs...)
	if prebuilts := a.overridableProperties.Prebuilts; len(prebuilts) > 0 {
		// For prebuilt_etc, use the first variant (64 on 64/32bit device, 32 on 32bit device)
		// regardless of the TARGET_PREFER_* setting. See b/144532908
		arches := ctx.DeviceConfig().Arches()
		if len(arches) != 0 {
			archForPrebuiltEtc := arches[0]
			for _, arch := range arches {
				// Prefer 64-bit arch if there is any
				if arch.ArchType.Multilib == "lib64" {
					archForPrebuiltEtc = arch
					break
				}
			}
			ctx.AddFarVariationDependencies([]blueprint.Variation{
				{Mutator: "os", Variation: ctx.Os().String()},
				{Mutator: "arch", Variation: archForPrebuiltEtc.String()},
			}, prebuiltTag, prebuilts...)
		}
	}

	// Dependencies for signing
	if String(a.overridableProperties.Key) == "" {
		ctx.PropertyErrorf("key", "missing")
		return
	}
	ctx.AddDependency(ctx.Module(), keyTag, String(a.overridableProperties.Key))

	cert := android.SrcIsModule(a.getCertString(ctx))
	if cert != "" {
		ctx.AddDependency(ctx.Module(), certificateTag, cert)
		// empty cert is not an error. Cert and private keys will be directly found under
		// PRODUCT_DEFAULT_DEV_CERTIFICATE
	}
}

func apexDCLADepsMutator(mctx android.BottomUpMutatorContext) {
	if !mctx.Config().ApexTrimEnabled() {
		return
	}
	if a, ok := mctx.Module().(*apexBundle); ok && a.overridableProperties.Trim_against != nil {
		commonVariation := mctx.Config().AndroidCommonTarget.Variations()
		mctx.AddFarVariationDependencies(commonVariation, dclaTag, String(a.overridableProperties.Trim_against))
	} else if o, ok := mctx.Module().(*OverrideApex); ok {
		for _, p := range o.GetProperties() {
			properties, ok := p.(*overridableProperties)
			if !ok {
				continue
			}
			if properties.Trim_against != nil {
				commonVariation := mctx.Config().AndroidCommonTarget.Variations()
				mctx.AddFarVariationDependencies(commonVariation, dclaTag, String(properties.Trim_against))
			}
		}
	}
}

type DCLAInfo struct {
	ProvidedLibs []string
}

var DCLAInfoProvider = blueprint.NewMutatorProvider(DCLAInfo{}, "apex_info")

type ApexBundleInfo struct {
	Contents *android.ApexContents
}

var ApexBundleInfoProvider = blueprint.NewMutatorProvider(ApexBundleInfo{}, "apex_info")

var _ ApexInfoMutator = (*apexBundle)(nil)

func (a *apexBundle) ApexVariationName() string {
	return a.properties.ApexVariationName
}

// ApexInfoMutator is responsible for collecting modules that need to have apex variants. They are
// identified by doing a graph walk starting from an apexBundle. Basically, all the (direct and
// indirect) dependencies are collected. But a few types of modules that shouldn't be included in
// the apexBundle (e.g. stub libraries) are not collected. Note that a single module can be depended
// on by multiple apexBundles. In that case, the module is collected for all of the apexBundles.
//
// For each dependency between an apex and an ApexModule an ApexInfo object describing the apex
// is passed to that module's BuildForApex(ApexInfo) method which collates them all in a list.
// The apexMutator uses that list to create module variants for the apexes to which it belongs.
// The relationship between module variants and apexes is not one-to-one as variants will be
// shared between compatible apexes.
func (a *apexBundle) ApexInfoMutator(mctx android.TopDownMutatorContext) {

	// The VNDK APEX is special. For the APEX, the membership is described in a very different
	// way. There is no dependency from the VNDK APEX to the VNDK libraries. Instead, VNDK
	// libraries are self-identified by their vndk.enabled properties. There is no need to run
	// this mutator for the APEX as nothing will be collected. So, let's return fast.
	if a.vndkApex {
		return
	}

	// Special casing for APEXes on non-system (e.g., vendor, odm, etc.) partitions. They are
	// provided with a property named use_vndk_as_stable, which when set to true doesn't collect
	// VNDK libraries as transitive dependencies. This option is useful for reducing the size of
	// the non-system APEXes because the VNDK libraries won't be included (and duped) in the
	// APEX, but shared across APEXes via the VNDK APEX.
	useVndk := a.SocSpecific() || a.DeviceSpecific() || (a.ProductSpecific() && mctx.Config().EnforceProductPartitionInterface())
	excludeVndkLibs := useVndk && proptools.Bool(a.properties.Use_vndk_as_stable)
	if proptools.Bool(a.properties.Use_vndk_as_stable) {
		if !useVndk {
			mctx.PropertyErrorf("use_vndk_as_stable", "not supported for system/system_ext APEXes")
		}
		if a.minSdkVersionValue(mctx) != "" {
			mctx.PropertyErrorf("use_vndk_as_stable", "not supported when min_sdk_version is set")
		}
		mctx.VisitDirectDepsWithTag(sharedLibTag, func(dep android.Module) {
			if c, ok := dep.(*cc.Module); ok && c.IsVndk() {
				mctx.PropertyErrorf("use_vndk_as_stable", "Trying to include a VNDK library(%s) while use_vndk_as_stable is true.", dep.Name())
			}
		})
		if mctx.Failed() {
			return
		}
	}

	continueApexDepsWalk := func(child, parent android.Module) bool {
		am, ok := child.(android.ApexModule)
		if !ok || !am.CanHaveApexVariants() {
			return false
		}
		depTag := mctx.OtherModuleDependencyTag(child)

		// Check to see if the tag always requires that the child module has an apex variant for every
		// apex variant of the parent module. If it does not then it is still possible for something
		// else, e.g. the DepIsInSameApex(...) method to decide that a variant is required.
		if required, ok := depTag.(android.AlwaysRequireApexVariantTag); ok && required.AlwaysRequireApexVariant() {
			return true
		}
		if !android.IsDepInSameApex(mctx, parent, child) {
			return false
		}
		if excludeVndkLibs {
			if c, ok := child.(*cc.Module); ok && c.IsVndk() {
				return false
			}
		}
		// By default, all the transitive dependencies are collected, unless filtered out
		// above.
		return true
	}

	// Records whether a certain module is included in this apexBundle via direct dependency or
	// inndirect dependency.
	contents := make(map[string]android.ApexMembership)
	mctx.WalkDeps(func(child, parent android.Module) bool {
		if !continueApexDepsWalk(child, parent) {
			return false
		}
		// If the parent is apexBundle, this child is directly depended.
		_, directDep := parent.(*apexBundle)
		depName := mctx.OtherModuleName(child)
		contents[depName] = contents[depName].Add(directDep)
		return true
	})

	// The membership information is saved for later access
	apexContents := android.NewApexContents(contents)
	mctx.SetProvider(ApexBundleInfoProvider, ApexBundleInfo{
		Contents: apexContents,
	})

	minSdkVersion := a.minSdkVersion(mctx)
	// When min_sdk_version is not set, the apex is built against FutureApiLevel.
	if minSdkVersion.IsNone() {
		minSdkVersion = android.FutureApiLevel
	}

	// This is the main part of this mutator. Mark the collected dependencies that they need to
	// be built for this apexBundle.

	apexVariationName := mctx.ModuleName() // could be com.android.foo
	a.properties.ApexVariationName = apexVariationName
	apexInfo := android.ApexInfo{
		ApexVariationName: apexVariationName,
		MinSdkVersion:     minSdkVersion,
		Updatable:         a.Updatable(),
		UsePlatformApis:   a.UsePlatformApis(),
		InApexVariants:    []string{apexVariationName},
		InApexModules:     []string{a.Name()}, // could be com.mycompany.android.foo
		ApexContents:      []*android.ApexContents{apexContents},
	}
	mctx.WalkDeps(func(child, parent android.Module) bool {
		if !continueApexDepsWalk(child, parent) {
			return false
		}
		child.(android.ApexModule).BuildForApex(apexInfo) // leave a mark!
		return true
	})

	if a.dynamic_common_lib_apex() {
		mctx.SetProvider(DCLAInfoProvider, DCLAInfo{
			ProvidedLibs: a.properties.Native_shared_libs,
		})
	}
}

type ApexInfoMutator interface {
	// ApexVariationName returns the name of the APEX variation to use in the apex
	// mutator etc. It is the same name as ApexInfo.ApexVariationName.
	ApexVariationName() string

	// ApexInfoMutator implementations must call BuildForApex(ApexInfo) on any modules that are
	// depended upon by an apex and which require an apex specific variant.
	ApexInfoMutator(android.TopDownMutatorContext)
}

// apexInfoMutator delegates the work of identifying which modules need an ApexInfo and apex
// specific variant to modules that support the ApexInfoMutator.
// It also propagates updatable=true to apps of updatable apexes
func apexInfoMutator(mctx android.TopDownMutatorContext) {
	if !mctx.Module().Enabled() {
		return
	}

	if a, ok := mctx.Module().(ApexInfoMutator); ok {
		a.ApexInfoMutator(mctx)
	}
	enforceAppUpdatability(mctx)
}

// apexStrictUpdatibilityLintMutator propagates strict_updatability_linting to transitive deps of a mainline module
// This check is enforced for updatable modules
func apexStrictUpdatibilityLintMutator(mctx android.TopDownMutatorContext) {
	if !mctx.Module().Enabled() {
		return
	}
	if apex, ok := mctx.Module().(*apexBundle); ok && apex.checkStrictUpdatabilityLinting() {
		mctx.WalkDeps(func(child, parent android.Module) bool {
			// b/208656169 Do not propagate strict updatability linting to libcore/
			// These libs are available on the classpath during compilation
			// These libs are transitive deps of the sdk. See java/sdk.go:decodeSdkDep
			// Only skip libraries defined in libcore root, not subdirectories
			if mctx.OtherModuleDir(child) == "libcore" {
				// Do not traverse transitive deps of libcore/ libs
				return false
			}
			if android.InList(child.Name(), skipLintJavalibAllowlist) {
				return false
			}
			if lintable, ok := child.(java.LintDepSetsIntf); ok {
				lintable.SetStrictUpdatabilityLinting(true)
			}
			// visit transitive deps
			return true
		})
	}
}

// enforceAppUpdatability propagates updatable=true to apps of updatable apexes
func enforceAppUpdatability(mctx android.TopDownMutatorContext) {
	if !mctx.Module().Enabled() {
		return
	}
	if apex, ok := mctx.Module().(*apexBundle); ok && apex.Updatable() {
		// checking direct deps is sufficient since apex->apk is a direct edge, even when inherited via apex_defaults
		mctx.VisitDirectDeps(func(module android.Module) {
			// ignore android_test_app
			if app, ok := module.(*java.AndroidApp); ok {
				app.SetUpdatable(true)
			}
		})
	}
}

// TODO: b/215736885 Whittle the denylist
// Transitive deps of certain mainline modules baseline NewApi errors
// Skip these mainline modules for now
var (
	skipStrictUpdatabilityLintAllowlist = []string{
		"com.android.art",
		"com.android.art.debug",
		"com.android.conscrypt",
		"com.android.media",
		// test apexes
		"test_com.android.art",
		"test_com.android.conscrypt",
		"test_com.android.media",
		"test_jitzygote_com.android.art",
	}

	// TODO: b/215736885 Remove this list
	skipLintJavalibAllowlist = []string{
		"conscrypt.module.platform.api.stubs",
		"conscrypt.module.public.api.stubs",
		"conscrypt.module.public.api.stubs.system",
		"conscrypt.module.public.api.stubs.module_lib",
		"framework-media.stubs",
		"framework-media.stubs.system",
		"framework-media.stubs.module_lib",
	}
)

func (a *apexBundle) checkStrictUpdatabilityLinting() bool {
	return a.Updatable() && !android.InList(a.ApexVariationName(), skipStrictUpdatabilityLintAllowlist)
}

// apexUniqueVariationsMutator checks if any dependencies use unique apex variations. If so, use
// unique apex variations for this module. See android/apex.go for more about unique apex variant.
// TODO(jiyong): move this to android/apex.go?
func apexUniqueVariationsMutator(mctx android.BottomUpMutatorContext) {
	if !mctx.Module().Enabled() {
		return
	}
	if am, ok := mctx.Module().(android.ApexModule); ok {
		android.UpdateUniqueApexVariationsForDeps(mctx, am)
	}
}

// apexTestForDepsMutator checks if this module is a test for an apex. If so, add a dependency on
// the apex in order to retrieve its contents later.
// TODO(jiyong): move this to android/apex.go?
func apexTestForDepsMutator(mctx android.BottomUpMutatorContext) {
	if !mctx.Module().Enabled() {
		return
	}
	if am, ok := mctx.Module().(android.ApexModule); ok {
		if testFor := am.TestFor(); len(testFor) > 0 {
			mctx.AddFarVariationDependencies([]blueprint.Variation{
				{Mutator: "os", Variation: am.Target().OsVariation()},
				{"arch", "common"},
			}, testForTag, testFor...)
		}
	}
}

// TODO(jiyong): move this to android/apex.go?
func apexTestForMutator(mctx android.BottomUpMutatorContext) {
	if !mctx.Module().Enabled() {
		return
	}
	if _, ok := mctx.Module().(android.ApexModule); ok {
		var contents []*android.ApexContents
		for _, testFor := range mctx.GetDirectDepsWithTag(testForTag) {
			abInfo := mctx.OtherModuleProvider(testFor, ApexBundleInfoProvider).(ApexBundleInfo)
			contents = append(contents, abInfo.Contents)
		}
		mctx.SetProvider(android.ApexTestForInfoProvider, android.ApexTestForInfo{
			ApexContents: contents,
		})
	}
}

// markPlatformAvailability marks whether or not a module can be available to platform. A module
// cannot be available to platform if 1) it is explicitly marked as not available (i.e.
// "//apex_available:platform" is absent) or 2) it depends on another module that isn't (or can't
// be) available to platform
// TODO(jiyong): move this to android/apex.go?
func markPlatformAvailability(mctx android.BottomUpMutatorContext) {
	// Host and recovery are not considered as platform
	if mctx.Host() || mctx.Module().InstallInRecovery() {
		return
	}

	am, ok := mctx.Module().(android.ApexModule)
	if !ok {
		return
	}

	availableToPlatform := am.AvailableFor(android.AvailableToPlatform)

	// If any of the dep is not available to platform, this module is also considered as being
	// not available to platform even if it has "//apex_available:platform"
	mctx.VisitDirectDeps(func(child android.Module) {
		if !android.IsDepInSameApex(mctx, am, child) {
			// if the dependency crosses apex boundary, don't consider it
			return
		}
		if dep, ok := child.(android.ApexModule); ok && dep.NotAvailableForPlatform() {
			availableToPlatform = false
			// TODO(b/154889534) trigger an error when 'am' has
			// "//apex_available:platform"
		}
	})

	// Exception 1: check to see if the module always requires it.
	if am.AlwaysRequiresPlatformApexVariant() {
		availableToPlatform = true
	}

	// Exception 2: bootstrap bionic libraries are also always available to platform
	if cc.InstallToBootstrap(mctx.ModuleName(), mctx.Config()) {
		availableToPlatform = true
	}

	if !availableToPlatform {
		am.SetNotAvailableForPlatform()
	}
}

// apexMutator visits each module and creates apex variations if the module was marked in the
// previous run of apexInfoMutator.
func apexMutator(mctx android.BottomUpMutatorContext) {
	if !mctx.Module().Enabled() {
		return
	}

	// This is the usual path.
	if am, ok := mctx.Module().(android.ApexModule); ok && am.CanHaveApexVariants() {
		android.CreateApexVariations(mctx, am)
		return
	}

	// apexBundle itself is mutated so that it and its dependencies have the same apex variant.
	if ai, ok := mctx.Module().(ApexInfoMutator); ok && apexModuleTypeRequiresVariant(ai) {
		apexBundleName := ai.ApexVariationName()
		mctx.CreateVariations(apexBundleName)
		if strings.HasPrefix(apexBundleName, "com.android.art") {
			// Create an alias from the platform variant. This is done to make
			// test_for dependencies work for modules that are split by the APEX
			// mutator, since test_for dependencies always go to the platform variant.
			// This doesn't happen for normal APEXes that are disjunct, so only do
			// this for the overlapping ART APEXes.
			// TODO(b/183882457): Remove this if the test_for functionality is
			// refactored to depend on the proper APEX variants instead of platform.
			mctx.CreateAliasVariation("", apexBundleName)
		}
	} else if o, ok := mctx.Module().(*OverrideApex); ok {
		apexBundleName := o.GetOverriddenModuleName()
		if apexBundleName == "" {
			mctx.ModuleErrorf("base property is not set")
			return
		}
		mctx.CreateVariations(apexBundleName)
		if strings.HasPrefix(apexBundleName, "com.android.art") {
			// TODO(b/183882457): See note for CreateAliasVariation above.
			mctx.CreateAliasVariation("", apexBundleName)
		}
	}
}

// apexModuleTypeRequiresVariant determines whether the module supplied requires an apex specific
// variant.
func apexModuleTypeRequiresVariant(module ApexInfoMutator) bool {
	if a, ok := module.(*apexBundle); ok {
		// TODO(jiyong): document the reason why the VNDK APEX is an exception here.
		return !a.vndkApex
	}

	return true
}

// See android.UpdateDirectlyInAnyApex
// TODO(jiyong): move this to android/apex.go?
func apexDirectlyInAnyMutator(mctx android.BottomUpMutatorContext) {
	if !mctx.Module().Enabled() {
		return
	}
	if am, ok := mctx.Module().(android.ApexModule); ok {
		android.UpdateDirectlyInAnyApex(mctx, am)
	}
}

// apexPackaging represents a specific packaging method for an APEX.
type apexPackaging int

const (
	// imageApex is a packaging method where contents are included in a filesystem image which
	// is then included in a zip container. This is the most typical way of packaging.
	imageApex apexPackaging = iota

	// zipApex is a packaging method where contents are directly included in the zip container.
	// This is used for host-side testing - because the contents are easily accessible by
	// unzipping the container.
	zipApex

	// flattendApex is a packaging method where contents are not included in the APEX file, but
	// installed to /apex/<apexname> directory on the device. This packaging method is used for
	// old devices where the filesystem-based APEX file can't be supported.
	flattenedApex
)

const (
	// File extensions of an APEX for different packaging methods
	imageApexSuffix  = ".apex"
	imageCapexSuffix = ".capex"
	zipApexSuffix    = ".zipapex"
	flattenedSuffix  = ".flattened"

	// variant names each of which is for a packaging method
	imageApexType     = "image"
	zipApexType       = "zip"
	flattenedApexType = "flattened"

	ext4FsType  = "ext4"
	f2fsFsType  = "f2fs"
	erofsFsType = "erofs"
)

// The suffix for the output "file", not the module
func (a apexPackaging) suffix() string {
	switch a {
	case imageApex:
		return imageApexSuffix
	case zipApex:
		return zipApexSuffix
	default:
		panic(fmt.Errorf("unknown APEX type %d", a))
	}
}

func (a apexPackaging) name() string {
	switch a {
	case imageApex:
		return imageApexType
	case zipApex:
		return zipApexType
	default:
		panic(fmt.Errorf("unknown APEX type %d", a))
	}
}

// apexFlattenedMutator creates one or more variations each of which is for a packaging method.
// TODO(jiyong): give a better name to this mutator
func apexFlattenedMutator(mctx android.BottomUpMutatorContext) {
	if !mctx.Module().Enabled() {
		return
	}
	if ab, ok := mctx.Module().(*apexBundle); ok {
		var variants []string
		switch proptools.StringDefault(ab.properties.Payload_type, "image") {
		case "image":
			// This is the normal case. Note that both image and flattend APEXes are
			// created. The image type is installed to the system partition, while the
			// flattened APEX is (optionally) installed to the system_ext partition.
			// This is mostly for GSI which has to support wide range of devices. If GSI
			// is installed on a newer (APEX-capable) device, the image APEX in the
			// system will be used. However, if the same GSI is installed on an old
			// device which can't support image APEX, the flattened APEX in the
			// system_ext partion (which still is part of GSI) is used instead.
			variants = append(variants, imageApexType, flattenedApexType)
		case "zip":
			variants = append(variants, zipApexType)
		case "both":
			variants = append(variants, imageApexType, zipApexType, flattenedApexType)
		default:
			mctx.PropertyErrorf("payload_type", "%q is not one of \"image\", \"zip\", or \"both\".", *ab.properties.Payload_type)
			return
		}

		modules := mctx.CreateLocalVariations(variants...)

		for i, v := range variants {
			switch v {
			case imageApexType:
				modules[i].(*apexBundle).properties.ApexType = imageApex
			case zipApexType:
				modules[i].(*apexBundle).properties.ApexType = zipApex
			case flattenedApexType:
				modules[i].(*apexBundle).properties.ApexType = flattenedApex
				// See the comment above for why system_ext.
				if !mctx.Config().FlattenApex() && ab.Platform() {
					modules[i].(*apexBundle).MakeAsSystemExt()
				}
			}
		}
	} else if _, ok := mctx.Module().(*OverrideApex); ok {
		// payload_type is forcibly overridden to "image"
		// TODO(jiyong): is this the right decision?
		mctx.CreateVariations(imageApexType, flattenedApexType)
	}
}

var _ android.DepIsInSameApex = (*apexBundle)(nil)

// Implements android.DepInInSameApex
func (a *apexBundle) DepIsInSameApex(_ android.BaseModuleContext, _ android.Module) bool {
	// direct deps of an APEX bundle are all part of the APEX bundle
	// TODO(jiyong): shouldn't we look into the payload field of the dependencyTag?
	return true
}

var _ android.OutputFileProducer = (*apexBundle)(nil)

// Implements android.OutputFileProducer
func (a *apexBundle) OutputFiles(tag string) (android.Paths, error) {
	switch tag {
	case "", android.DefaultDistTag:
		// This is the default dist path.
		return android.Paths{a.outputFile}, nil
	case imageApexSuffix:
		// uncompressed one
		if a.outputApexFile != nil {
			return android.Paths{a.outputApexFile}, nil
		}
		fallthrough
	default:
		return nil, fmt.Errorf("unsupported module reference tag %q", tag)
	}
}

var _ multitree.Exportable = (*apexBundle)(nil)

func (a *apexBundle) Exportable() bool {
	if a.properties.ApexType == flattenedApex {
		return false
	}
	return true
}

func (a *apexBundle) TaggedOutputs() map[string]android.Paths {
	ret := make(map[string]android.Paths)
	ret["apex"] = android.Paths{a.outputFile}
	return ret
}

var _ cc.Coverage = (*apexBundle)(nil)

// Implements cc.Coverage
func (a *apexBundle) IsNativeCoverageNeeded(ctx android.BaseModuleContext) bool {
	return ctx.Device() && ctx.DeviceConfig().NativeCoverageEnabled()
}

// Implements cc.Coverage
func (a *apexBundle) SetPreventInstall() {
	a.properties.PreventInstall = true
}

// Implements cc.Coverage
func (a *apexBundle) HideFromMake() {
	a.properties.HideFromMake = true
	// This HideFromMake is shadowing the ModuleBase one, call through to it for now.
	// TODO(ccross): untangle these
	a.ModuleBase.HideFromMake()
}

// Implements cc.Coverage
func (a *apexBundle) MarkAsCoverageVariant(coverage bool) {
	a.properties.IsCoverageVariant = coverage
}

// Implements cc.Coverage
func (a *apexBundle) EnableCoverageIfNeeded() {}

var _ android.ApexBundleDepsInfoIntf = (*apexBundle)(nil)

// Implements android.ApexBundleDepsInfoIntf
func (a *apexBundle) Updatable() bool {
	return proptools.BoolDefault(a.properties.Updatable, true)
}

func (a *apexBundle) FutureUpdatable() bool {
	return proptools.BoolDefault(a.properties.Future_updatable, false)
}

func (a *apexBundle) UsePlatformApis() bool {
	return proptools.BoolDefault(a.properties.Platform_apis, false)
}

// getCertString returns the name of the cert that should be used to sign this APEX. This is
// basically from the "certificate" property, but could be overridden by the device config.
func (a *apexBundle) getCertString(ctx android.BaseModuleContext) string {
	moduleName := ctx.ModuleName()
	// VNDK APEXes share the same certificate. To avoid adding a new VNDK version to the
	// OVERRIDE_* list, we check with the pseudo module name to see if its certificate is
	// overridden.
	if a.vndkApex {
		moduleName = vndkApexName
	}
	certificate, overridden := ctx.DeviceConfig().OverrideCertificateFor(moduleName)
	if overridden {
		return ":" + certificate
	}
	return String(a.overridableProperties.Certificate)
}

// See the installable property
func (a *apexBundle) installable() bool {
	return !a.properties.PreventInstall && (a.properties.Installable == nil || proptools.Bool(a.properties.Installable))
}

// See the generate_hashtree property
func (a *apexBundle) shouldGenerateHashtree() bool {
	return proptools.BoolDefault(a.properties.Generate_hashtree, true)
}

// See the test_only_unsigned_payload property
func (a *apexBundle) testOnlyShouldSkipPayloadSign() bool {
	return proptools.Bool(a.properties.Test_only_unsigned_payload)
}

// See the test_only_force_compression property
func (a *apexBundle) testOnlyShouldForceCompression() bool {
	return proptools.Bool(a.properties.Test_only_force_compression)
}

// See the dynamic_common_lib_apex property
func (a *apexBundle) dynamic_common_lib_apex() bool {
	return proptools.BoolDefault(a.properties.Dynamic_common_lib_apex, false)
}

// See the list of libs to trim
func (a *apexBundle) libs_to_trim(ctx android.ModuleContext) []string {
	dclaModules := ctx.GetDirectDepsWithTag(dclaTag)
	if len(dclaModules) > 1 {
		panic(fmt.Errorf("expected exactly at most one dcla dependency, got %d", len(dclaModules)))
	}
	if len(dclaModules) > 0 {
		DCLAInfo := ctx.OtherModuleProvider(dclaModules[0], DCLAInfoProvider).(DCLAInfo)
		return DCLAInfo.ProvidedLibs
	}
	return []string{}
}

// These functions are interfacing with cc/sanitizer.go. The entire APEX (along with all of its
// members) can be sanitized, either forcibly, or by the global configuration. For some of the
// sanitizers, extra dependencies can be forcibly added as well.

func (a *apexBundle) EnableSanitizer(sanitizerName string) {
	if !android.InList(sanitizerName, a.properties.SanitizerNames) {
		a.properties.SanitizerNames = append(a.properties.SanitizerNames, sanitizerName)
	}
}

func (a *apexBundle) IsSanitizerEnabled(config android.Config, sanitizerName string) bool {
	if android.InList(sanitizerName, a.properties.SanitizerNames) {
		return true
	}

	// Then follow the global setting
	var globalSanitizerNames []string
	if a.Host() {
		globalSanitizerNames = config.SanitizeHost()
	} else {
		arches := config.SanitizeDeviceArch()
		if len(arches) == 0 || android.InList(a.Arch().ArchType.Name, arches) {
			globalSanitizerNames = config.SanitizeDevice()
		}
	}
	return android.InList(sanitizerName, globalSanitizerNames)
}

func (a *apexBundle) AddSanitizerDependencies(ctx android.BottomUpMutatorContext, sanitizerName string) {
	// TODO(jiyong): move this info (the sanitizer name, the lib name, etc.) to cc/sanitize.go
	// Keep only the mechanism here.
	if ctx.Device() && sanitizerName == "hwaddress" && strings.HasPrefix(a.Name(), "com.android.runtime") {
		imageVariation := a.getImageVariation(ctx)
		for _, target := range ctx.MultiTargets() {
			if target.Arch.ArchType.Multilib == "lib64" {
				addDependenciesForNativeModules(ctx, ApexNativeDependencies{
					Native_shared_libs: []string{"libclang_rt.hwasan"},
					Tests:              nil,
					Jni_libs:           nil,
					Binaries:           nil,
				}, target, imageVariation)
				break
			}
		}
	}
}

// apexFileFor<Type> functions below create an apexFile struct for a given Soong module. The
// returned apexFile saves information about the Soong module that will be used for creating the
// build rules.
func apexFileForNativeLibrary(ctx android.BaseModuleContext, ccMod *cc.Module, handleSpecialLibs bool) apexFile {
	// Decide the APEX-local directory by the multilib of the library In the future, we may
	// query this to the module.
	// TODO(jiyong): use the new PackagingSpec
	var dirInApex string
	switch ccMod.Arch().ArchType.Multilib {
	case "lib32":
		dirInApex = "lib"
	case "lib64":
		dirInApex = "lib64"
	}
	if ccMod.Target().NativeBridge == android.NativeBridgeEnabled {
		dirInApex = filepath.Join(dirInApex, ccMod.Target().NativeBridgeRelativePath)
	}
	if handleSpecialLibs && cc.InstallToBootstrap(ccMod.BaseModuleName(), ctx.Config()) {
		// Special case for Bionic libs and other libs installed with them. This is to
		// prevent those libs from being included in the search path
		// /apex/com.android.runtime/${LIB}. This exclusion is required because those libs
		// in the Runtime APEX are available via the legacy paths in /system/lib/. By the
		// init process, the libs in the APEX are bind-mounted to the legacy paths and thus
		// will be loaded into the default linker namespace (aka "platform" namespace). If
		// the libs are directly in /apex/com.android.runtime/${LIB} then the same libs will
		// be loaded again into the runtime linker namespace, which will result in double
		// loading of them, which isn't supported.
		dirInApex = filepath.Join(dirInApex, "bionic")
	}
	// This needs to go after the runtime APEX handling because otherwise we would get
	// weird paths like lib64/rel_install_path/bionic rather than
	// lib64/bionic/rel_install_path.
	dirInApex = filepath.Join(dirInApex, ccMod.RelativeInstallPath())

	fileToCopy := android.OutputFileForModule(ctx, ccMod, "")
	androidMkModuleName := ccMod.BaseModuleName() + ccMod.Properties.SubName
	return newApexFile(ctx, fileToCopy, androidMkModuleName, dirInApex, nativeSharedLib, ccMod)
}

func apexFileForExecutable(ctx android.BaseModuleContext, cc *cc.Module) apexFile {
	dirInApex := "bin"
	if cc.Target().NativeBridge == android.NativeBridgeEnabled {
		dirInApex = filepath.Join(dirInApex, cc.Target().NativeBridgeRelativePath)
	}
	dirInApex = filepath.Join(dirInApex, cc.RelativeInstallPath())
	fileToCopy := android.OutputFileForModule(ctx, cc, "")
	androidMkModuleName := cc.BaseModuleName() + cc.Properties.SubName
	af := newApexFile(ctx, fileToCopy, androidMkModuleName, dirInApex, nativeExecutable, cc)
	af.symlinks = cc.Symlinks()
	af.dataPaths = cc.DataPaths()
	return af
}

func apexFileForRustExecutable(ctx android.BaseModuleContext, rustm *rust.Module) apexFile {
	dirInApex := "bin"
	if rustm.Target().NativeBridge == android.NativeBridgeEnabled {
		dirInApex = filepath.Join(dirInApex, rustm.Target().NativeBridgeRelativePath)
	}
	fileToCopy := android.OutputFileForModule(ctx, rustm, "")
	androidMkModuleName := rustm.BaseModuleName() + rustm.Properties.SubName
	af := newApexFile(ctx, fileToCopy, androidMkModuleName, dirInApex, nativeExecutable, rustm)
	return af
}

func apexFileForRustLibrary(ctx android.BaseModuleContext, rustm *rust.Module) apexFile {
	// Decide the APEX-local directory by the multilib of the library
	// In the future, we may query this to the module.
	var dirInApex string
	switch rustm.Arch().ArchType.Multilib {
	case "lib32":
		dirInApex = "lib"
	case "lib64":
		dirInApex = "lib64"
	}
	if rustm.Target().NativeBridge == android.NativeBridgeEnabled {
		dirInApex = filepath.Join(dirInApex, rustm.Target().NativeBridgeRelativePath)
	}
	fileToCopy := android.OutputFileForModule(ctx, rustm, "")
	androidMkModuleName := rustm.BaseModuleName() + rustm.Properties.SubName
	return newApexFile(ctx, fileToCopy, androidMkModuleName, dirInApex, nativeSharedLib, rustm)
}

func apexFileForPyBinary(ctx android.BaseModuleContext, py *python.PythonBinaryModule) apexFile {
	dirInApex := "bin"
	fileToCopy := py.HostToolPath().Path()
	return newApexFile(ctx, fileToCopy, py.BaseModuleName(), dirInApex, pyBinary, py)
}

func apexFileForGoBinary(ctx android.BaseModuleContext, depName string, gb bootstrap.GoBinaryTool) apexFile {
	dirInApex := "bin"
	fileToCopy := android.PathForGoBinary(ctx, gb)
	// NB: Since go binaries are static we don't need the module for anything here, which is
	// good since the go tool is a blueprint.Module not an android.Module like we would
	// normally use.
	//
	return newApexFile(ctx, fileToCopy, depName, dirInApex, goBinary, nil)
}

func apexFileForShBinary(ctx android.BaseModuleContext, sh *sh.ShBinary) apexFile {
	dirInApex := filepath.Join("bin", sh.SubDir())
	if sh.Target().NativeBridge == android.NativeBridgeEnabled {
		dirInApex = filepath.Join(dirInApex, sh.Target().NativeBridgeRelativePath)
	}
	fileToCopy := sh.OutputFile()
	af := newApexFile(ctx, fileToCopy, sh.BaseModuleName(), dirInApex, shBinary, sh)
	af.symlinks = sh.Symlinks()
	return af
}

func apexFileForPrebuiltEtc(ctx android.BaseModuleContext, prebuilt prebuilt_etc.PrebuiltEtcModule, depName string) apexFile {
	dirInApex := filepath.Join(prebuilt.BaseDir(), prebuilt.SubDir())
	fileToCopy := prebuilt.OutputFile()
	return newApexFile(ctx, fileToCopy, depName, dirInApex, etc, prebuilt)
}

func apexFileForCompatConfig(ctx android.BaseModuleContext, config java.PlatformCompatConfigIntf, depName string) apexFile {
	dirInApex := filepath.Join("etc", config.SubDir())
	fileToCopy := config.CompatConfig()
	return newApexFile(ctx, fileToCopy, depName, dirInApex, etc, config)
}

// javaModule is an interface to handle all Java modules (java_library, dex_import, etc) in the same
// way.
type javaModule interface {
	android.Module
	BaseModuleName() string
	DexJarBuildPath() java.OptionalDexJarPath
	JacocoReportClassesFile() android.Path
	LintDepSets() java.LintDepSets
	Stem() string
}

var _ javaModule = (*java.Library)(nil)
var _ javaModule = (*java.Import)(nil)
var _ javaModule = (*java.SdkLibrary)(nil)
var _ javaModule = (*java.DexImport)(nil)
var _ javaModule = (*java.SdkLibraryImport)(nil)

// apexFileForJavaModule creates an apexFile for a java module's dex implementation jar.
func apexFileForJavaModule(ctx android.BaseModuleContext, module javaModule) apexFile {
	return apexFileForJavaModuleWithFile(ctx, module, module.DexJarBuildPath().PathOrNil())
}

// apexFileForJavaModuleWithFile creates an apexFile for a java module with the supplied file.
func apexFileForJavaModuleWithFile(ctx android.BaseModuleContext, module javaModule, dexImplementationJar android.Path) apexFile {
	dirInApex := "javalib"
	af := newApexFile(ctx, dexImplementationJar, module.BaseModuleName(), dirInApex, javaSharedLib, module)
	af.jacocoReportClassesFile = module.JacocoReportClassesFile()
	af.lintDepSets = module.LintDepSets()
	af.customStem = module.Stem() + ".jar"
	if dexpreopter, ok := module.(java.DexpreopterInterface); ok {
		for _, install := range dexpreopter.DexpreoptBuiltInstalledForApex() {
			af.requiredModuleNames = append(af.requiredModuleNames, install.FullModuleName())
		}
	}
	return af
}

func apexFileForJavaModuleProfile(ctx android.BaseModuleContext, module javaModule) *apexFile {
	if dexpreopter, ok := module.(java.DexpreopterInterface); ok {
		if profilePathOnHost := dexpreopter.OutputProfilePathOnHost(); profilePathOnHost != nil {
			dirInApex := "javalib"
			af := newApexFile(ctx, profilePathOnHost, module.BaseModuleName()+"-profile", dirInApex, etc, nil)
			af.customStem = module.Stem() + ".jar.prof"
			return &af
		}
	}
	return nil
}

// androidApp is an interface to handle all app modules (android_app, android_app_import, etc.) in
// the same way.
type androidApp interface {
	android.Module
	Privileged() bool
	InstallApkName() string
	OutputFile() android.Path
	JacocoReportClassesFile() android.Path
	Certificate() java.Certificate
	BaseModuleName() string
	LintDepSets() java.LintDepSets
}

var _ androidApp = (*java.AndroidApp)(nil)
var _ androidApp = (*java.AndroidAppImport)(nil)

func sanitizedBuildIdForPath(ctx android.BaseModuleContext) string {
	buildId := ctx.Config().BuildId()

	// The build ID is used as a suffix for a filename, so ensure that
	// the set of characters being used are sanitized.
	// - any word character: [a-zA-Z0-9_]
	// - dots: .
	// - dashes: -
	validRegex := regexp.MustCompile(`^[\w\.\-\_]+$`)
	if !validRegex.MatchString(buildId) {
		ctx.ModuleErrorf("Unable to use build id %s as filename suffix, valid characters are [a-z A-Z 0-9 _ . -].", buildId)
	}
	return buildId
}

func apexFileForAndroidApp(ctx android.BaseModuleContext, aapp androidApp) apexFile {
	appDir := "app"
	if aapp.Privileged() {
		appDir = "priv-app"
	}

	// TODO(b/224589412, b/226559955): Ensure that the subdirname is suffixed
	// so that PackageManager correctly invalidates the existing installed apk
	// in favour of the new APK-in-APEX.  See bugs for more information.
	dirInApex := filepath.Join(appDir, aapp.InstallApkName()+"@"+sanitizedBuildIdForPath(ctx))
	fileToCopy := aapp.OutputFile()

	af := newApexFile(ctx, fileToCopy, aapp.BaseModuleName(), dirInApex, app, aapp)
	af.jacocoReportClassesFile = aapp.JacocoReportClassesFile()
	af.lintDepSets = aapp.LintDepSets()
	af.certificate = aapp.Certificate()

	if app, ok := aapp.(interface {
		OverriddenManifestPackageName() string
	}); ok {
		af.overriddenPackageName = app.OverriddenManifestPackageName()
	}
	return af
}

func apexFileForRuntimeResourceOverlay(ctx android.BaseModuleContext, rro java.RuntimeResourceOverlayModule) apexFile {
	rroDir := "overlay"
	dirInApex := filepath.Join(rroDir, rro.Theme())
	fileToCopy := rro.OutputFile()
	af := newApexFile(ctx, fileToCopy, rro.Name(), dirInApex, app, rro)
	af.certificate = rro.Certificate()

	if a, ok := rro.(interface {
		OverriddenManifestPackageName() string
	}); ok {
		af.overriddenPackageName = a.OverriddenManifestPackageName()
	}
	return af
}

func apexFileForBpfProgram(ctx android.BaseModuleContext, builtFile android.Path, apex_sub_dir string, bpfProgram bpf.BpfModule) apexFile {
	dirInApex := filepath.Join("etc", "bpf", apex_sub_dir)
	return newApexFile(ctx, builtFile, builtFile.Base(), dirInApex, etc, bpfProgram)
}

func apexFileForFilesystem(ctx android.BaseModuleContext, buildFile android.Path, fs filesystem.Filesystem) apexFile {
	dirInApex := filepath.Join("etc", "fs")
	return newApexFile(ctx, buildFile, buildFile.Base(), dirInApex, etc, fs)
}

// WalkPayloadDeps visits dependencies that contributes to the payload of this APEX. For each of the
// visited module, the `do` callback is executed. Returning true in the callback continues the visit
// to the child modules. Returning false makes the visit to continue in the sibling or the parent
// modules. This is used in check* functions below.
func (a *apexBundle) WalkPayloadDeps(ctx android.ModuleContext, do android.PayloadDepsCallback) {
	ctx.WalkDeps(func(child, parent android.Module) bool {
		am, ok := child.(android.ApexModule)
		if !ok || !am.CanHaveApexVariants() {
			return false
		}

		// Filter-out unwanted depedendencies
		depTag := ctx.OtherModuleDependencyTag(child)
		if _, ok := depTag.(android.ExcludeFromApexContentsTag); ok {
			return false
		}
		if dt, ok := depTag.(*dependencyTag); ok && !dt.payload {
			return false
		}

		ai := ctx.OtherModuleProvider(child, android.ApexInfoProvider).(android.ApexInfo)
		externalDep := !android.InList(ctx.ModuleName(), ai.InApexVariants)

		// Visit actually
		return do(ctx, parent, am, externalDep)
	})
}

// filesystem type of the apex_payload.img inside the APEX. Currently, ext4 and f2fs are supported.
type fsType int

const (
	ext4 fsType = iota
	f2fs
	erofs
)

func (f fsType) string() string {
	switch f {
	case ext4:
		return ext4FsType
	case f2fs:
		return f2fsFsType
	case erofs:
		return erofsFsType
	default:
		panic(fmt.Errorf("unknown APEX payload type %d", f))
	}
}

var _ android.MixedBuildBuildable = (*apexBundle)(nil)

func (a *apexBundle) IsMixedBuildSupported(ctx android.BaseModuleContext) bool {
	return a.properties.ApexType == imageApex
}

func (a *apexBundle) QueueBazelCall(ctx android.BaseModuleContext) {
	bazelCtx := ctx.Config().BazelContext
	bazelCtx.QueueBazelRequest(a.GetBazelLabel(ctx, a), cquery.GetApexInfo, android.GetConfigKey(ctx))
}

// GetBazelLabel returns the bazel label of this apexBundle, or the label of the
// override_apex module overriding this apexBundle. An apexBundle can be
// overridden by different override_apex modules (e.g. Google or Go variants),
// which is handled by the overrides mutators.
func (a *apexBundle) GetBazelLabel(ctx android.BazelConversionPathContext, module blueprint.Module) string {
	if _, ok := ctx.Module().(android.OverridableModule); ok {
		return android.MaybeBp2buildLabelOfOverridingModule(ctx)
	}
	return a.BazelModuleBase.GetBazelLabel(ctx, a)
}

func (a *apexBundle) ProcessBazelQueryResponse(ctx android.ModuleContext) {
	if !a.commonBuildActions(ctx) {
		return
	}

	a.setApexTypeAndSuffix(ctx)
	a.setPayloadFsType(ctx)
	a.setSystemLibLink(ctx)

	if a.properties.ApexType != zipApex {
		a.compatSymlinks = makeCompatSymlinks(a.BaseModuleName(), ctx, a.primaryApexType)
	}

	bazelCtx := ctx.Config().BazelContext
	outputs, err := bazelCtx.GetApexInfo(a.GetBazelLabel(ctx, a), android.GetConfigKey(ctx))
	if err != nil {
		ctx.ModuleErrorf(err.Error())
		return
	}
	a.installDir = android.PathForModuleInstall(ctx, "apex")

	// Set the output file to .apex or .capex depending on the compression configuration.
	a.setCompression(ctx)
	if a.isCompressed {
		a.outputApexFile = android.PathForBazelOutRelative(ctx, ctx.ModuleDir(), outputs.SignedCompressedOutput)
	} else {
		a.outputApexFile = android.PathForBazelOutRelative(ctx, ctx.ModuleDir(), outputs.SignedOutput)
	}
	a.outputFile = a.outputApexFile

	if len(outputs.TidyFiles) > 0 {
		tidyFiles := android.PathsForBazelOut(ctx, outputs.TidyFiles)
		a.outputFile = android.AttachValidationActions(ctx, a.outputFile, tidyFiles)
	}

	// TODO(b/257829940): These are used by the apex_keys_text singleton; would probably be a clearer
	// interface if these were set in a provider rather than the module itself
	a.publicKeyFile = android.PathForBazelOut(ctx, outputs.BundleKeyInfo[0])
	a.privateKeyFile = android.PathForBazelOut(ctx, outputs.BundleKeyInfo[1])
	a.containerCertificateFile = android.PathForBazelOut(ctx, outputs.ContainerKeyInfo[0])
	a.containerPrivateKeyFile = android.PathForBazelOut(ctx, outputs.ContainerKeyInfo[1])

	// Ensure ApexMkInfo.install_to_system make module names are installed as
	// part of a bundled build.
	a.makeModulesToInstall = append(a.makeModulesToInstall, outputs.MakeModulesToInstall...)

	apexType := a.properties.ApexType
	switch apexType {
	case imageApex:
		a.bundleModuleFile = android.PathForBazelOut(ctx, outputs.BundleFile)
		a.nativeApisUsedByModuleFile = android.ModuleOutPath(android.PathForBazelOut(ctx, outputs.SymbolsUsedByApex))
		a.nativeApisBackedByModuleFile = android.ModuleOutPath(android.PathForBazelOut(ctx, outputs.BackingLibs))
		// TODO(b/239084755): Generate the java api using.xml file from Bazel.
		a.javaApisUsedByModuleFile = android.ModuleOutPath(android.PathForBazelOut(ctx, outputs.JavaSymbolsUsedByApex))
		a.installedFilesFile = android.ModuleOutPath(android.PathForBazelOut(ctx, outputs.InstalledFiles))
		installSuffix := imageApexSuffix
		if a.isCompressed {
			installSuffix = imageCapexSuffix
		}
		a.installedFile = ctx.InstallFile(a.installDir, a.Name()+installSuffix, a.outputFile,
			a.compatSymlinks.Paths()...)
	default:
		panic(fmt.Errorf("internal error: unexpected apex_type for the ProcessBazelQueryResponse: %v", a.properties.ApexType))
	}

	// filesInfo in mixed mode must retrieve all information about the apex's
	// contents completely from the Starlark providers. It should never rely on
	// Android.bp information, as they might not exist for fully migrated
	// dependencies.
	//
	// Prevent accidental writes to filesInfo in the earlier parts Soong by
	// asserting it to be nil.
	if a.filesInfo != nil {
		panic(
			fmt.Errorf("internal error: filesInfo must be nil for an apex handled by Bazel. " +
				"Did something else set filesInfo before this line of code?"))
	}
	for _, f := range outputs.PayloadFilesInfo {
		fileInfo := apexFile{
			isBazelPrebuilt: true,

			builtFile:           android.PathForBazelOut(ctx, f["built_file"]),
			unstrippedBuiltFile: android.PathForBazelOut(ctx, f["unstripped_built_file"]),
			androidMkModuleName: f["make_module_name"],
			installDir:          f["install_dir"],
			class:               classes[f["class"]],
			customStem:          f["basename"],
			moduleDir:           f["package"],
		}

		arch := f["arch"]
		fileInfo.arch = arch
		if len(arch) > 0 {
			fileInfo.multilib = "lib32"
			if strings.HasSuffix(arch, "64") {
				fileInfo.multilib = "lib64"
			}
		}

		a.filesInfo = append(a.filesInfo, fileInfo)
	}
}

func (a *apexBundle) setCompression(ctx android.ModuleContext) {
	if a.properties.ApexType != imageApex {
		a.isCompressed = false
	} else if a.testOnlyShouldForceCompression() {
		a.isCompressed = true
	} else {
		a.isCompressed = ctx.Config().ApexCompressionEnabled() && a.isCompressable()
	}
}

func (a *apexBundle) setSystemLibLink(ctx android.ModuleContext) {
	// Optimization. If we are building bundled APEX, for the files that are gathered due to the
	// transitive dependencies, don't place them inside the APEX, but place a symlink pointing
	// the same library in the system partition, thus effectively sharing the same libraries
	// across the APEX boundary. For unbundled APEX, all the gathered files are actually placed
	// in the APEX.
	a.linkToSystemLib = !ctx.Config().UnbundledBuild() && a.installable()

	// APEXes targeting other than system/system_ext partitions use vendor/product variants.
	// So we can't link them to /system/lib libs which are core variants.
	if a.SocSpecific() || a.DeviceSpecific() || (a.ProductSpecific() && ctx.Config().EnforceProductPartitionInterface()) {
		a.linkToSystemLib = false
	}

	forced := ctx.Config().ForceApexSymlinkOptimization()
	updatable := a.Updatable() || a.FutureUpdatable()

	// We don't need the optimization for updatable APEXes, as it might give false signal
	// to the system health when the APEXes are still bundled (b/149805758).
	if !forced && updatable && a.properties.ApexType == imageApex {
		a.linkToSystemLib = false
	}

	// We also don't want the optimization for host APEXes, because it doesn't make sense.
	if ctx.Host() {
		a.linkToSystemLib = false
	}
}

func (a *apexBundle) setPayloadFsType(ctx android.ModuleContext) {
	switch proptools.StringDefault(a.properties.Payload_fs_type, ext4FsType) {
	case ext4FsType:
		a.payloadFsType = ext4
	case f2fsFsType:
		a.payloadFsType = f2fs
	case erofsFsType:
		a.payloadFsType = erofs
	default:
		ctx.PropertyErrorf("payload_fs_type", "%q is not a valid filesystem for apex [ext4, f2fs, erofs]", *a.properties.Payload_fs_type)
	}
}

func (a *apexBundle) setApexTypeAndSuffix(ctx android.ModuleContext) {
	// Set suffix and primaryApexType depending on the ApexType
	buildFlattenedAsDefault := ctx.Config().FlattenApex()
	switch a.properties.ApexType {
	case imageApex:
		if buildFlattenedAsDefault {
			a.suffix = imageApexSuffix
		} else {
			a.suffix = ""
			a.primaryApexType = true

			if ctx.Config().InstallExtraFlattenedApexes() {
				a.makeModulesToInstall = append(a.makeModulesToInstall, a.Name()+flattenedSuffix)
			}
		}
	case zipApex:
		if proptools.String(a.properties.Payload_type) == "zip" {
			a.suffix = ""
			a.primaryApexType = true
		} else {
			a.suffix = zipApexSuffix
		}
	case flattenedApex:
		if buildFlattenedAsDefault {
			a.suffix = ""
			a.primaryApexType = true
		} else {
			a.suffix = flattenedSuffix
		}
	}
}

func (a apexBundle) isCompressable() bool {
	return proptools.BoolDefault(a.overridableProperties.Compressible, false) && !a.testApex
}

func (a *apexBundle) commonBuildActions(ctx android.ModuleContext) bool {
	a.checkApexAvailability(ctx)
	a.checkUpdatable(ctx)
	a.CheckMinSdkVersion(ctx)
	a.checkStaticLinkingToStubLibraries(ctx)
	a.checkStaticExecutables(ctx)
	if len(a.properties.Tests) > 0 && !a.testApex {
		ctx.PropertyErrorf("tests", "property allowed only in apex_test module type")
		return false
	}
	return true
}

type visitorContext struct {
	// all the files that will be included in this APEX
	filesInfo []apexFile

	// native lib dependencies
	provideNativeLibs []string
	requireNativeLibs []string

	handleSpecialLibs bool

	// if true, raise error on duplicate apexFile
	checkDuplicate bool
}

func (vctx *visitorContext) normalizeFileInfo(mctx android.ModuleContext) {
	encountered := make(map[string]apexFile)
	for _, f := range vctx.filesInfo {
		dest := filepath.Join(f.installDir, f.builtFile.Base())
		if e, ok := encountered[dest]; !ok {
			encountered[dest] = f
		} else {
			if vctx.checkDuplicate && f.builtFile.String() != e.builtFile.String() {
				mctx.ModuleErrorf("apex file %v is provided by two different files %v and %v",
					dest, e.builtFile, f.builtFile)
				return
			}
			// If a module is directly included and also transitively depended on
			// consider it as directly included.
			e.transitiveDep = e.transitiveDep && f.transitiveDep
			encountered[dest] = e
		}
	}
	vctx.filesInfo = vctx.filesInfo[:0]
	for _, v := range encountered {
		vctx.filesInfo = append(vctx.filesInfo, v)
	}
	sort.Slice(vctx.filesInfo, func(i, j int) bool {
		// Sort by destination path so as to ensure consistent ordering even if the source of the files
		// changes.
		return vctx.filesInfo[i].path() < vctx.filesInfo[j].path()
	})
}

func (a *apexBundle) depVisitor(vctx *visitorContext, ctx android.ModuleContext, child, parent blueprint.Module) bool {
	depTag := ctx.OtherModuleDependencyTag(child)
	if _, ok := depTag.(android.ExcludeFromApexContentsTag); ok {
		return false
	}
	if mod, ok := child.(android.Module); ok && !mod.Enabled() {
		return false
	}
	depName := ctx.OtherModuleName(child)
	if _, isDirectDep := parent.(*apexBundle); isDirectDep {
		switch depTag {
		case sharedLibTag, jniLibTag:
			isJniLib := depTag == jniLibTag
			switch ch := child.(type) {
			case *cc.Module:
				fi := apexFileForNativeLibrary(ctx, ch, vctx.handleSpecialLibs)
				fi.isJniLib = isJniLib
				vctx.filesInfo = append(vctx.filesInfo, fi)
				// Collect the list of stub-providing libs except:
				// - VNDK libs are only for vendors
				// - bootstrap bionic libs are treated as provided by system
				if ch.HasStubsVariants() && !a.vndkApex && !cc.InstallToBootstrap(ch.BaseModuleName(), ctx.Config()) {
					vctx.provideNativeLibs = append(vctx.provideNativeLibs, fi.stem())
				}
				return true // track transitive dependencies
			case *rust.Module:
				fi := apexFileForRustLibrary(ctx, ch)
				fi.isJniLib = isJniLib
				vctx.filesInfo = append(vctx.filesInfo, fi)
				return true // track transitive dependencies
			default:
				propertyName := "native_shared_libs"
				if isJniLib {
					propertyName = "jni_libs"
				}
				ctx.PropertyErrorf(propertyName, "%q is not a cc_library or cc_library_shared module", depName)
			}
		case executableTag:
			switch ch := child.(type) {
			case *cc.Module:
				vctx.filesInfo = append(vctx.filesInfo, apexFileForExecutable(ctx, ch))
				return true // track transitive dependencies
			case *python.PythonBinaryModule:
				if ch.HostToolPath().Valid() {
					vctx.filesInfo = append(vctx.filesInfo, apexFileForPyBinary(ctx, ch))
				}
			case bootstrap.GoBinaryTool:
				if a.Host() {
					vctx.filesInfo = append(vctx.filesInfo, apexFileForGoBinary(ctx, depName, ch))
				}
			case *rust.Module:
				vctx.filesInfo = append(vctx.filesInfo, apexFileForRustExecutable(ctx, ch))
				return true // track transitive dependencies
			default:
				ctx.PropertyErrorf("binaries",
					"%q is neither cc_binary, rust_binary, (embedded) py_binary, (host) blueprint_go_binary, nor (host) bootstrap_go_binary", depName)
			}
		case shBinaryTag:
			if csh, ok := child.(*sh.ShBinary); ok {
				vctx.filesInfo = append(vctx.filesInfo, apexFileForShBinary(ctx, csh))
			} else {
				ctx.PropertyErrorf("sh_binaries", "%q is not a sh_binary module", depName)
			}
		case bcpfTag:
			_, ok := child.(*java.BootclasspathFragmentModule)
			if !ok {
				ctx.PropertyErrorf("bootclasspath_fragments", "%q is not a bootclasspath_fragment module", depName)
				return false
			}

			vctx.filesInfo = append(vctx.filesInfo, apexBootclasspathFragmentFiles(ctx, child)...)
			return true
		case sscpfTag:
			if _, ok := child.(*java.SystemServerClasspathModule); !ok {
				ctx.PropertyErrorf("systemserverclasspath_fragments",
					"%q is not a systemserverclasspath_fragment module", depName)
				return false
			}
			if af := apexClasspathFragmentProtoFile(ctx, child); af != nil {
				vctx.filesInfo = append(vctx.filesInfo, *af)
			}
			return true
		case javaLibTag:
			switch child.(type) {
			case *java.Library, *java.SdkLibrary, *java.DexImport, *java.SdkLibraryImport, *java.Import:
				af := apexFileForJavaModule(ctx, child.(javaModule))
				if !af.ok() {
					ctx.PropertyErrorf("java_libs", "%q is not configured to be compiled into dex", depName)
					return false
				}
				vctx.filesInfo = append(vctx.filesInfo, af)
				return true // track transitive dependencies
			default:
				ctx.PropertyErrorf("java_libs", "%q of type %q is not supported", depName, ctx.OtherModuleType(child))
			}
		case androidAppTag:
			switch ap := child.(type) {
			case *java.AndroidApp:
				vctx.filesInfo = append(vctx.filesInfo, apexFileForAndroidApp(ctx, ap))
				return true // track transitive dependencies
			case *java.AndroidAppImport:
				vctx.filesInfo = append(vctx.filesInfo, apexFileForAndroidApp(ctx, ap))
			case *java.AndroidTestHelperApp:
				vctx.filesInfo = append(vctx.filesInfo, apexFileForAndroidApp(ctx, ap))
			case *java.AndroidAppSet:
				appDir := "app"
				if ap.Privileged() {
					appDir = "priv-app"
				}
				// TODO(b/224589412, b/226559955): Ensure that the dirname is
				// suffixed so that PackageManager correctly invalidates the
				// existing installed apk in favour of the new APK-in-APEX.
				// See bugs for more information.
				appDirName := filepath.Join(appDir, ap.BaseModuleName()+"@"+sanitizedBuildIdForPath(ctx))
				af := newApexFile(ctx, ap.OutputFile(), ap.BaseModuleName(), appDirName, appSet, ap)
				af.certificate = java.PresignedCertificate
				vctx.filesInfo = append(vctx.filesInfo, af)
			default:
				ctx.PropertyErrorf("apps", "%q is not an android_app module", depName)
			}
		case rroTag:
			if rro, ok := child.(java.RuntimeResourceOverlayModule); ok {
				vctx.filesInfo = append(vctx.filesInfo, apexFileForRuntimeResourceOverlay(ctx, rro))
			} else {
				ctx.PropertyErrorf("rros", "%q is not an runtime_resource_overlay module", depName)
			}
		case bpfTag:
			if bpfProgram, ok := child.(bpf.BpfModule); ok {
				filesToCopy, _ := bpfProgram.OutputFiles("")
				apex_sub_dir := bpfProgram.SubDir()
				for _, bpfFile := range filesToCopy {
					vctx.filesInfo = append(vctx.filesInfo, apexFileForBpfProgram(ctx, bpfFile, apex_sub_dir, bpfProgram))
				}
			} else {
				ctx.PropertyErrorf("bpfs", "%q is not a bpf module", depName)
			}
		case fsTag:
			if fs, ok := child.(filesystem.Filesystem); ok {
				vctx.filesInfo = append(vctx.filesInfo, apexFileForFilesystem(ctx, fs.OutputPath(), fs))
			} else {
				ctx.PropertyErrorf("filesystems", "%q is not a filesystem module", depName)
			}
		case prebuiltTag:
			if prebuilt, ok := child.(prebuilt_etc.PrebuiltEtcModule); ok {
				vctx.filesInfo = append(vctx.filesInfo, apexFileForPrebuiltEtc(ctx, prebuilt, depName))
			} else {
				ctx.PropertyErrorf("prebuilts", "%q is not a prebuilt_etc module", depName)
			}
		case compatConfigTag:
			if compatConfig, ok := child.(java.PlatformCompatConfigIntf); ok {
				vctx.filesInfo = append(vctx.filesInfo, apexFileForCompatConfig(ctx, compatConfig, depName))
			} else {
				ctx.PropertyErrorf("compat_configs", "%q is not a platform_compat_config module", depName)
			}
		case testTag:
			if ccTest, ok := child.(*cc.Module); ok {
				if ccTest.IsTestPerSrcAllTestsVariation() {
					// Multiple-output test module (where `test_per_src: true`).
					//
					// `ccTest` is the "" ("all tests") variation of a `test_per_src` module.
					// We do not add this variation to `filesInfo`, as it has no output;
					// however, we do add the other variations of this module as indirect
					// dependencies (see below).
				} else {
					// Single-output test module (where `test_per_src: false`).
					af := apexFileForExecutable(ctx, ccTest)
					af.class = nativeTest
					vctx.filesInfo = append(vctx.filesInfo, af)
				}
				return true // track transitive dependencies
			} else {
				ctx.PropertyErrorf("tests", "%q is not a cc module", depName)
			}
		case keyTag:
			if key, ok := child.(*apexKey); ok {
				a.privateKeyFile = key.privateKeyFile
				a.publicKeyFile = key.publicKeyFile
			} else {
				ctx.PropertyErrorf("key", "%q is not an apex_key module", depName)
			}
		case certificateTag:
			if dep, ok := child.(*java.AndroidAppCertificate); ok {
				a.containerCertificateFile = dep.Certificate.Pem
				a.containerPrivateKeyFile = dep.Certificate.Key
			} else {
				ctx.ModuleErrorf("certificate dependency %q must be an android_app_certificate module", depName)
			}
		}
		return false
	}

	if a.vndkApex {
		return false
	}

	// indirect dependencies
	am, ok := child.(android.ApexModule)
	if !ok {
		return false
	}
	// We cannot use a switch statement on `depTag` here as the checked
	// tags used below are private (e.g. `cc.sharedDepTag`).
	if cc.IsSharedDepTag(depTag) || cc.IsRuntimeDepTag(depTag) {
		if ch, ok := child.(*cc.Module); ok {
			if ch.UseVndk() && proptools.Bool(a.properties.Use_vndk_as_stable) && ch.IsVndk() {
				vctx.requireNativeLibs = append(vctx.requireNativeLibs, ":vndk")
				return false
			}
			af := apexFileForNativeLibrary(ctx, ch, vctx.handleSpecialLibs)
			af.transitiveDep = true

			// Always track transitive dependencies for host.
			if a.Host() {
				vctx.filesInfo = append(vctx.filesInfo, af)
				return true
			}

			abInfo := ctx.Provider(ApexBundleInfoProvider).(ApexBundleInfo)
			if !abInfo.Contents.DirectlyInApex(depName) && (ch.IsStubs() || ch.HasStubsVariants()) {
				// If the dependency is a stubs lib, don't include it in this APEX,
				// but make sure that the lib is installed on the device.
				// In case no APEX is having the lib, the lib is installed to the system
				// partition.
				//
				// Always include if we are a host-apex however since those won't have any
				// system libraries.
				//
				// Skip the dependency in unbundled builds where the device image is not
				// being built.
				if ch.IsStubsImplementationRequired() && !am.DirectlyInAnyApex() && !ctx.Config().UnbundledBuild() {
					// we need a module name for Make
					name := ch.ImplementationModuleNameForMake(ctx) + ch.Properties.SubName
					if !android.InList(name, a.makeModulesToInstall) {
						a.makeModulesToInstall = append(a.makeModulesToInstall, name)
					}
				}
				vctx.requireNativeLibs = append(vctx.requireNativeLibs, af.stem())
				// Don't track further
				return false
			}

			// If the dep is not considered to be in the same
			// apex, don't add it to filesInfo so that it is not
			// included in this APEX.
			// TODO(jiyong): move this to at the top of the
			// else-if clause for the indirect dependencies.
			// Currently, that's impossible because we would
			// like to record requiredNativeLibs even when
			// DepIsInSameAPex is false. We also shouldn't do
			// this for host.
			//
			// TODO(jiyong): explain why the same module is passed in twice.
			// Switching the first am to parent breaks lots of tests.
			if !android.IsDepInSameApex(ctx, am, am) {
				return false
			}

			vctx.filesInfo = append(vctx.filesInfo, af)
			return true // track transitive dependencies
		} else if rm, ok := child.(*rust.Module); ok {
			af := apexFileForRustLibrary(ctx, rm)
			af.transitiveDep = true
			vctx.filesInfo = append(vctx.filesInfo, af)
			return true // track transitive dependencies
		}
	} else if cc.IsTestPerSrcDepTag(depTag) {
		if ch, ok := child.(*cc.Module); ok {
			af := apexFileForExecutable(ctx, ch)
			// Handle modules created as `test_per_src` variations of a single test module:
			// use the name of the generated test binary (`fileToCopy`) instead of the name
			// of the original test module (`depName`, shared by all `test_per_src`
			// variations of that module).
			af.androidMkModuleName = filepath.Base(af.builtFile.String())
			// these are not considered transitive dep
			af.transitiveDep = false
			vctx.filesInfo = append(vctx.filesInfo, af)
			return true // track transitive dependencies
		}
	} else if cc.IsHeaderDepTag(depTag) {
		// nothing
	} else if java.IsJniDepTag(depTag) {
		// Because APK-in-APEX embeds jni_libs transitively, we don't need to track transitive deps
	} else if java.IsXmlPermissionsFileDepTag(depTag) {
		if prebuilt, ok := child.(prebuilt_etc.PrebuiltEtcModule); ok {
			vctx.filesInfo = append(vctx.filesInfo, apexFileForPrebuiltEtc(ctx, prebuilt, depName))
		}
	} else if rust.IsDylibDepTag(depTag) {
		if rustm, ok := child.(*rust.Module); ok && rustm.IsInstallableToApex() {
			af := apexFileForRustLibrary(ctx, rustm)
			af.transitiveDep = true
			vctx.filesInfo = append(vctx.filesInfo, af)
			return true // track transitive dependencies
		}
	} else if rust.IsRlibDepTag(depTag) {
		// Rlib is statically linked, but it might have shared lib
		// dependencies. Track them.
		return true
	} else if java.IsBootclasspathFragmentContentDepTag(depTag) {
		// Add the contents of the bootclasspath fragment to the apex.
		switch child.(type) {
		case *java.Library, *java.SdkLibrary:
			javaModule := child.(javaModule)
			af := apexFileForBootclasspathFragmentContentModule(ctx, parent, javaModule)
			if !af.ok() {
				ctx.PropertyErrorf("bootclasspath_fragments",
					"bootclasspath_fragment content %q is not configured to be compiled into dex", depName)
				return false
			}
			vctx.filesInfo = append(vctx.filesInfo, af)
			return true // track transitive dependencies
		default:
			ctx.PropertyErrorf("bootclasspath_fragments",
				"bootclasspath_fragment content %q of type %q is not supported", depName, ctx.OtherModuleType(child))
		}
	} else if java.IsSystemServerClasspathFragmentContentDepTag(depTag) {
		// Add the contents of the systemserverclasspath fragment to the apex.
		switch child.(type) {
		case *java.Library, *java.SdkLibrary:
			af := apexFileForJavaModule(ctx, child.(javaModule))
			vctx.filesInfo = append(vctx.filesInfo, af)
			if profileAf := apexFileForJavaModuleProfile(ctx, child.(javaModule)); profileAf != nil {
				vctx.filesInfo = append(vctx.filesInfo, *profileAf)
			}
			return true // track transitive dependencies
		default:
			ctx.PropertyErrorf("systemserverclasspath_fragments",
				"systemserverclasspath_fragment content %q of type %q is not supported", depName, ctx.OtherModuleType(child))
		}
	} else if _, ok := depTag.(android.CopyDirectlyInAnyApexTag); ok {
		// nothing
	} else if depTag == android.DarwinUniversalVariantTag {
		// nothing
	} else if am.CanHaveApexVariants() && am.IsInstallableToApex() {
		ctx.ModuleErrorf("unexpected tag %s for indirect dependency %q", android.PrettyPrintTag(depTag), depName)
	}
	return false
}

func (a *apexBundle) shouldCheckDuplicate(ctx android.ModuleContext) bool {
	// TODO(b/263308293) remove this
	if a.properties.IsCoverageVariant {
		return false
	}
	// TODO(b/263308515) remove this
	if a.testApex {
		return false
	}
	// TODO(b/263309864) remove this
	if a.Host() {
		return false
	}
	if a.Device() && ctx.DeviceConfig().DeviceArch() == "" {
		return false
	}
	return true
}

// Creates build rules for an APEX. It consists of the following major steps:
//
// 1) do some validity checks such as apex_available, min_sdk_version, etc.
// 2) traverse the dependency tree to collect apexFile structs from them.
// 3) some fields in apexBundle struct are configured
// 4) generate the build rules to create the APEX. This is mostly done in builder.go.
func (a *apexBundle) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	////////////////////////////////////////////////////////////////////////////////////////////
	// 1) do some validity checks such as apex_available, min_sdk_version, etc.
	if !a.commonBuildActions(ctx) {
		return
	}
	////////////////////////////////////////////////////////////////////////////////////////////
	// 2) traverse the dependency tree to collect apexFile structs from them.
	// Collect the module directory for IDE info in java/jdeps.go.
	a.modulePaths = append(a.modulePaths, ctx.ModuleDir())

	// TODO(jiyong): do this using WalkPayloadDeps
	// TODO(jiyong): make this clean!!!
	vctx := visitorContext{
		handleSpecialLibs: !android.Bool(a.properties.Ignore_system_library_special_case),
		checkDuplicate:    a.shouldCheckDuplicate(ctx),
	}
	ctx.WalkDepsBlueprint(func(child, parent blueprint.Module) bool { return a.depVisitor(&vctx, ctx, child, parent) })
	vctx.normalizeFileInfo(ctx)
	if a.privateKeyFile == nil {
		ctx.PropertyErrorf("key", "private_key for %q could not be found", String(a.overridableProperties.Key))
		return
	}

	////////////////////////////////////////////////////////////////////////////////////////////
	// 3) some fields in apexBundle struct are configured
	a.installDir = android.PathForModuleInstall(ctx, "apex")
	a.filesInfo = vctx.filesInfo

	a.setApexTypeAndSuffix(ctx)
	a.setPayloadFsType(ctx)
	a.setSystemLibLink(ctx)
	if a.properties.ApexType != zipApex {
		a.compatSymlinks = makeCompatSymlinks(a.BaseModuleName(), ctx, a.primaryApexType)
	}

	////////////////////////////////////////////////////////////////////////////////////////////
	// 4) generate the build rules to create the APEX. This is done in builder.go.
	a.buildManifest(ctx, vctx.provideNativeLibs, vctx.requireNativeLibs)
	if a.properties.ApexType == flattenedApex {
		a.buildFlattenedApex(ctx)
	} else {
		a.buildUnflattenedApex(ctx)
	}
	a.buildApexDependencyInfo(ctx)
	a.buildLintReports(ctx)

	// Append meta-files to the filesInfo list so that they are reflected in Android.mk as well.
	if a.installable() {
		// For flattened APEX, make sure that APEX manifest and apex_pubkey are also copied
		// along with other ordinary files. (Note that this is done by apexer for
		// non-flattened APEXes)
		a.filesInfo = append(a.filesInfo, newApexFile(ctx, a.manifestPbOut, "apex_manifest.pb", ".", etc, nil))

		// Place the public key as apex_pubkey. This is also done by apexer for
		// non-flattened APEXes case.
		// TODO(jiyong): Why do we need this CP rule?
		copiedPubkey := android.PathForModuleOut(ctx, "apex_pubkey")
		ctx.Build(pctx, android.BuildParams{
			Rule:   android.Cp,
			Input:  a.publicKeyFile,
			Output: copiedPubkey,
		})
		a.filesInfo = append(a.filesInfo, newApexFile(ctx, copiedPubkey, "apex_pubkey", ".", etc, nil))
	}
}

// apexBootclasspathFragmentFiles returns the list of apexFile structures defining the files that
// the bootclasspath_fragment contributes to the apex.
func apexBootclasspathFragmentFiles(ctx android.ModuleContext, module blueprint.Module) []apexFile {
	bootclasspathFragmentInfo := ctx.OtherModuleProvider(module, java.BootclasspathFragmentApexContentInfoProvider).(java.BootclasspathFragmentApexContentInfo)
	var filesToAdd []apexFile

	// Add classpaths.proto config.
	if af := apexClasspathFragmentProtoFile(ctx, module); af != nil {
		filesToAdd = append(filesToAdd, *af)
	}

	pathInApex := bootclasspathFragmentInfo.ProfileInstallPathInApex()
	if pathInApex != "" {
		pathOnHost := bootclasspathFragmentInfo.ProfilePathOnHost()
		tempPath := android.PathForModuleOut(ctx, "boot_image_profile", pathInApex)

		if pathOnHost != nil {
			// We need to copy the profile to a temporary path with the right filename because the apexer
			// will take the filename as is.
			ctx.Build(pctx, android.BuildParams{
				Rule:   android.Cp,
				Input:  pathOnHost,
				Output: tempPath,
			})
		} else {
			// At this point, the boot image profile cannot be generated. It is probably because the boot
			// image profile source file does not exist on the branch, or it is not available for the
			// current build target.
			// However, we cannot enforce the boot image profile to be generated because some build
			// targets (such as module SDK) do not need it. It is only needed when the APEX is being
			// built. Therefore, we create an error rule so that an error will occur at the ninja phase
			// only if the APEX is being built.
			ctx.Build(pctx, android.BuildParams{
				Rule:   android.ErrorRule,
				Output: tempPath,
				Args: map[string]string{
					"error": "Boot image profile cannot be generated",
				},
			})
		}

		androidMkModuleName := filepath.Base(pathInApex)
		af := newApexFile(ctx, tempPath, androidMkModuleName, filepath.Dir(pathInApex), etc, nil)
		filesToAdd = append(filesToAdd, af)
	}

	return filesToAdd
}

// apexClasspathFragmentProtoFile returns *apexFile structure defining the classpath.proto config that
// the module contributes to the apex; or nil if the proto config was not generated.
func apexClasspathFragmentProtoFile(ctx android.ModuleContext, module blueprint.Module) *apexFile {
	info := ctx.OtherModuleProvider(module, java.ClasspathFragmentProtoContentInfoProvider).(java.ClasspathFragmentProtoContentInfo)
	if !info.ClasspathFragmentProtoGenerated {
		return nil
	}
	classpathProtoOutput := info.ClasspathFragmentProtoOutput
	af := newApexFile(ctx, classpathProtoOutput, classpathProtoOutput.Base(), info.ClasspathFragmentProtoInstallDir.Rel(), etc, nil)
	return &af
}

// apexFileForBootclasspathFragmentContentModule creates an apexFile for a bootclasspath_fragment
// content module, i.e. a library that is part of the bootclasspath.
func apexFileForBootclasspathFragmentContentModule(ctx android.ModuleContext, fragmentModule blueprint.Module, javaModule javaModule) apexFile {
	bootclasspathFragmentInfo := ctx.OtherModuleProvider(fragmentModule, java.BootclasspathFragmentApexContentInfoProvider).(java.BootclasspathFragmentApexContentInfo)

	// Get the dexBootJar from the bootclasspath_fragment as that is responsible for performing the
	// hidden API encpding.
	dexBootJar, err := bootclasspathFragmentInfo.DexBootJarPathForContentModule(javaModule)
	if err != nil {
		ctx.ModuleErrorf("%s", err)
	}

	// Create an apexFile as for a normal java module but with the dex boot jar provided by the
	// bootclasspath_fragment.
	af := apexFileForJavaModuleWithFile(ctx, javaModule, dexBootJar)
	return af
}

///////////////////////////////////////////////////////////////////////////////////////////////////
// Factory functions
//

func newApexBundle() *apexBundle {
	module := &apexBundle{}

	module.AddProperties(&module.properties)
	module.AddProperties(&module.targetProperties)
	module.AddProperties(&module.archProperties)
	module.AddProperties(&module.overridableProperties)

	android.InitAndroidMultiTargetsArchModule(module, android.HostAndDeviceSupported, android.MultilibCommon)
	android.InitDefaultableModule(module)
	android.InitOverridableModule(module, &module.overridableProperties.Overrides)
	android.InitBazelModule(module)
	multitree.InitExportableModule(module)
	return module
}

func ApexBundleFactory(testApex bool) android.Module {
	bundle := newApexBundle()
	bundle.testApex = testApex
	return bundle
}

// apex_test is an APEX for testing. The difference from the ordinary apex module type is that
// certain compatibility checks such as apex_available are not done for apex_test.
func TestApexBundleFactory() android.Module {
	bundle := newApexBundle()
	bundle.testApex = true
	return bundle
}

// apex packages other modules into an APEX file which is a packaging format for system-level
// components like binaries, shared libraries, etc.
func BundleFactory() android.Module {
	return newApexBundle()
}

type Defaults struct {
	android.ModuleBase
	android.DefaultsModuleBase
}

// apex_defaults provides defaultable properties to other apex modules.
func DefaultsFactory() android.Module {
	module := &Defaults{}

	module.AddProperties(
		&apexBundleProperties{},
		&apexTargetBundleProperties{},
		&apexArchBundleProperties{},
		&overridableProperties{},
	)

	android.InitDefaultsModule(module)
	return module
}

type OverrideApex struct {
	android.ModuleBase
	android.OverrideModuleBase
	android.BazelModuleBase
}

func (o *OverrideApex) GenerateAndroidBuildActions(_ android.ModuleContext) {
	// All the overrides happen in the base module.
}

// override_apex is used to create an apex module based on another apex module by overriding some of
// its properties.
func OverrideApexFactory() android.Module {
	m := &OverrideApex{}

	m.AddProperties(&overridableProperties{})

	android.InitAndroidMultiTargetsArchModule(m, android.DeviceSupported, android.MultilibCommon)
	android.InitOverrideModule(m)
	android.InitBazelModule(m)
	return m
}

func (o *OverrideApex) ConvertWithBp2build(ctx android.TopDownMutatorContext) {
	if ctx.ModuleType() != "override_apex" {
		return
	}

	baseApexModuleName := o.OverrideModuleBase.GetOverriddenModuleName()
	baseModule, baseApexExists := ctx.ModuleFromName(baseApexModuleName)
	if !baseApexExists {
		panic(fmt.Errorf("Base apex module doesn't exist: %s", baseApexModuleName))
	}

	a, baseModuleIsApex := baseModule.(*apexBundle)
	if !baseModuleIsApex {
		panic(fmt.Errorf("Base module is not apex module: %s", baseApexModuleName))
	}
	attrs, props, commonAttrs := convertWithBp2build(a, ctx)

	// We just want the name, not module reference.
	baseApexName := strings.TrimPrefix(baseApexModuleName, ":")
	attrs.Base_apex_name = &baseApexName

	for _, p := range o.GetProperties() {
		overridableProperties, ok := p.(*overridableProperties)
		if !ok {
			continue
		}

		// Manifest is either empty or a file in the directory of base APEX and is not overridable.
		// After it is converted in convertWithBp2build(baseApex, ctx),
		// the attrs.Manifest.Value.Label is the file path relative to the directory
		// of base apex. So the following code converts it to a label that looks like
		// <package of base apex>:<path of manifest file> if base apex and override
		// apex are not in the same package.
		baseApexPackage := ctx.OtherModuleDir(a)
		overrideApexPackage := ctx.ModuleDir()
		if baseApexPackage != overrideApexPackage {
			attrs.Manifest.Value.Label = "//" + baseApexPackage + ":" + attrs.Manifest.Value.Label
		}

		// Key
		if overridableProperties.Key != nil {
			attrs.Key = bazel.LabelAttribute{}
			attrs.Key.SetValue(android.BazelLabelForModuleDepSingle(ctx, *overridableProperties.Key))
		}

		// Certificate
		if overridableProperties.Certificate == nil {
			// If overridableProperties.Certificate is nil, clear this out as
			// well with zeroed structs, so the override_apex does not use the
			// base apex's certificate.
			attrs.Certificate = bazel.LabelAttribute{}
			attrs.Certificate_name = bazel.StringAttribute{}
		} else {
			attrs.Certificate, attrs.Certificate_name = android.BazelStringOrLabelFromProp(ctx, overridableProperties.Certificate)
		}

		// Prebuilts
		if overridableProperties.Prebuilts != nil {
			prebuiltsLabelList := android.BazelLabelForModuleDeps(ctx, overridableProperties.Prebuilts)
			attrs.Prebuilts = bazel.MakeLabelListAttribute(prebuiltsLabelList)
		}

		// Compressible
		if overridableProperties.Compressible != nil {
			attrs.Compressible = bazel.BoolAttribute{Value: overridableProperties.Compressible}
		}

		// Package name
		//
		// e.g. com.android.adbd's package name is com.android.adbd, but
		// com.google.android.adbd overrides the package name to com.google.android.adbd
		//
		// TODO: this can be overridden from the product configuration, see
		// getOverrideManifestPackageName and
		// PRODUCT_MANIFEST_PACKAGE_NAME_OVERRIDES.
		//
		// Instead of generating the BUILD files differently based on the product config
		// at the point of conversion, this should be handled by the BUILD file loading
		// from the soong_injection's product_vars, so product config is decoupled from bp2build.
		if overridableProperties.Package_name != "" {
			attrs.Package_name = &overridableProperties.Package_name
		}

		// Logging parent
		if overridableProperties.Logging_parent != "" {
			attrs.Logging_parent = &overridableProperties.Logging_parent
		}
	}

	commonAttrs.Name = o.Name()

	ctx.CreateBazelTargetModule(props, commonAttrs, &attrs)
}

///////////////////////////////////////////////////////////////////////////////////////////////////
// Vality check routines
//
// These are called in at the very beginning of GenerateAndroidBuildActions to flag an error when
// certain conditions are not met.
//
// TODO(jiyong): move these checks to a separate go file.

var _ android.ModuleWithMinSdkVersionCheck = (*apexBundle)(nil)

// Ensures that min_sdk_version of the included modules are equal or less than the min_sdk_version
// of this apexBundle.
func (a *apexBundle) CheckMinSdkVersion(ctx android.ModuleContext) {
	if a.testApex || a.vndkApex {
		return
	}
	// apexBundle::minSdkVersion reports its own errors.
	minSdkVersion := a.minSdkVersion(ctx)
	android.CheckMinSdkVersion(ctx, minSdkVersion, a.WalkPayloadDeps)
}

// Returns apex's min_sdk_version string value, honoring overrides
func (a *apexBundle) minSdkVersionValue(ctx android.EarlyModuleContext) string {
	// Only override the minSdkVersion value on Apexes which already specify
	// a min_sdk_version (it's optional for non-updatable apexes), and that its
	// min_sdk_version value is lower than the one to override with.
	minApiLevel := minSdkVersionFromValue(ctx, proptools.String(a.properties.Min_sdk_version))
	if minApiLevel.IsNone() {
		return ""
	}

	overrideMinSdkValue := ctx.DeviceConfig().ApexGlobalMinSdkVersionOverride()
	overrideApiLevel := minSdkVersionFromValue(ctx, overrideMinSdkValue)
	if !overrideApiLevel.IsNone() && overrideApiLevel.CompareTo(minApiLevel) > 0 {
		minApiLevel = overrideApiLevel
	}

	return minApiLevel.String()
}

// Returns apex's min_sdk_version SdkSpec, honoring overrides
func (a *apexBundle) MinSdkVersion(ctx android.EarlyModuleContext) android.ApiLevel {
	return a.minSdkVersion(ctx)
}

// Returns apex's min_sdk_version ApiLevel, honoring overrides
func (a *apexBundle) minSdkVersion(ctx android.EarlyModuleContext) android.ApiLevel {
	return minSdkVersionFromValue(ctx, a.minSdkVersionValue(ctx))
}

// Construct ApiLevel object from min_sdk_version string value
func minSdkVersionFromValue(ctx android.EarlyModuleContext, value string) android.ApiLevel {
	if value == "" {
		return android.NoneApiLevel
	}
	apiLevel, err := android.ApiLevelFromUser(ctx, value)
	if err != nil {
		ctx.PropertyErrorf("min_sdk_version", "%s", err.Error())
		return android.NoneApiLevel
	}
	return apiLevel
}

// Ensures that a lib providing stub isn't statically linked
func (a *apexBundle) checkStaticLinkingToStubLibraries(ctx android.ModuleContext) {
	// Practically, we only care about regular APEXes on the device.
	if ctx.Host() || a.testApex || a.vndkApex {
		return
	}

	abInfo := ctx.Provider(ApexBundleInfoProvider).(ApexBundleInfo)

	a.WalkPayloadDeps(ctx, func(ctx android.ModuleContext, from blueprint.Module, to android.ApexModule, externalDep bool) bool {
		if ccm, ok := to.(*cc.Module); ok {
			apexName := ctx.ModuleName()
			fromName := ctx.OtherModuleName(from)
			toName := ctx.OtherModuleName(to)

			// If `to` is not actually in the same APEX as `from` then it does not need
			// apex_available and neither do any of its dependencies.
			//
			// It is ok to call DepIsInSameApex() directly from within WalkPayloadDeps().
			if am, ok := from.(android.DepIsInSameApex); ok && !am.DepIsInSameApex(ctx, to) {
				// As soon as the dependency graph crosses the APEX boundary, don't go further.
				return false
			}

			// The dynamic linker and crash_dump tool in the runtime APEX is the only
			// exception to this rule. It can't make the static dependencies dynamic
			// because it can't do the dynamic linking for itself.
			// Same rule should be applied to linkerconfig, because it should be executed
			// only with static linked libraries before linker is available with ld.config.txt
			if apexName == "com.android.runtime" && (fromName == "linker" || fromName == "crash_dump" || fromName == "linkerconfig") {
				return false
			}

			isStubLibraryFromOtherApex := ccm.HasStubsVariants() && !abInfo.Contents.DirectlyInApex(toName)
			if isStubLibraryFromOtherApex && !externalDep {
				ctx.ModuleErrorf("%q required by %q is a native library providing stub. "+
					"It shouldn't be included in this APEX via static linking. Dependency path: %s", to.String(), fromName, ctx.GetPathString(false))
			}

		}
		return true
	})
}

// checkUpdatable enforces APEX and its transitive dep properties to have desired values for updatable APEXes.
func (a *apexBundle) checkUpdatable(ctx android.ModuleContext) {
	if a.Updatable() {
		if a.minSdkVersionValue(ctx) == "" {
			ctx.PropertyErrorf("updatable", "updatable APEXes should set min_sdk_version as well")
		}
		if a.UsePlatformApis() {
			ctx.PropertyErrorf("updatable", "updatable APEXes can't use platform APIs")
		}
		if proptools.Bool(a.properties.Use_vndk_as_stable) {
			ctx.PropertyErrorf("use_vndk_as_stable", "updatable APEXes can't use external VNDK libs")
		}
		if a.FutureUpdatable() {
			ctx.PropertyErrorf("future_updatable", "Already updatable. Remove `future_updatable: true:`")
		}
		a.checkJavaStableSdkVersion(ctx)
		a.checkClasspathFragments(ctx)
	}
}

// checkClasspathFragments enforces that all classpath fragments in deps generate classpaths.proto config.
func (a *apexBundle) checkClasspathFragments(ctx android.ModuleContext) {
	ctx.VisitDirectDeps(func(module android.Module) {
		if tag := ctx.OtherModuleDependencyTag(module); tag == bcpfTag || tag == sscpfTag {
			info := ctx.OtherModuleProvider(module, java.ClasspathFragmentProtoContentInfoProvider).(java.ClasspathFragmentProtoContentInfo)
			if !info.ClasspathFragmentProtoGenerated {
				ctx.OtherModuleErrorf(module, "is included in updatable apex %v, it must not set generate_classpaths_proto to false", ctx.ModuleName())
			}
		}
	})
}

// checkJavaStableSdkVersion enforces that all Java deps are using stable SDKs to compile.
func (a *apexBundle) checkJavaStableSdkVersion(ctx android.ModuleContext) {
	// Visit direct deps only. As long as we guarantee top-level deps are using stable SDKs,
	// java's checkLinkType guarantees correct usage for transitive deps
	ctx.VisitDirectDepsBlueprint(func(module blueprint.Module) {
		tag := ctx.OtherModuleDependencyTag(module)
		switch tag {
		case javaLibTag, androidAppTag:
			if m, ok := module.(interface {
				CheckStableSdkVersion(ctx android.BaseModuleContext) error
			}); ok {
				if err := m.CheckStableSdkVersion(ctx); err != nil {
					ctx.ModuleErrorf("cannot depend on \"%v\": %v", ctx.OtherModuleName(module), err)
				}
			}
		}
	})
}

// checkApexAvailability ensures that the all the dependencies are marked as available for this APEX.
func (a *apexBundle) checkApexAvailability(ctx android.ModuleContext) {
	// Let's be practical. Availability for test, host, and the VNDK apex isn't important
	if ctx.Host() || a.testApex || a.vndkApex {
		return
	}

	// Because APEXes targeting other than system/system_ext partitions can't set
	// apex_available, we skip checks for these APEXes
	if a.SocSpecific() || a.DeviceSpecific() || (a.ProductSpecific() && ctx.Config().EnforceProductPartitionInterface()) {
		return
	}

	// Coverage build adds additional dependencies for the coverage-only runtime libraries.
	// Requiring them and their transitive depencies with apex_available is not right
	// because they just add noise.
	if ctx.Config().IsEnvTrue("EMMA_INSTRUMENT") || a.IsNativeCoverageNeeded(ctx) {
		return
	}

	a.WalkPayloadDeps(ctx, func(ctx android.ModuleContext, from blueprint.Module, to android.ApexModule, externalDep bool) bool {
		// As soon as the dependency graph crosses the APEX boundary, don't go further.
		if externalDep {
			return false
		}

		apexName := ctx.ModuleName()
		fromName := ctx.OtherModuleName(from)
		toName := ctx.OtherModuleName(to)

		// If `to` is not actually in the same APEX as `from` then it does not need
		// apex_available and neither do any of its dependencies.
		//
		// It is ok to call DepIsInSameApex() directly from within WalkPayloadDeps().
		if am, ok := from.(android.DepIsInSameApex); ok && !am.DepIsInSameApex(ctx, to) {
			// As soon as the dependency graph crosses the APEX boundary, don't go
			// further.
			return false
		}

		if to.AvailableFor(apexName) || baselineApexAvailable(apexName, toName) {
			return true
		}
		ctx.ModuleErrorf("%q requires %q that doesn't list the APEX under 'apex_available'."+
			"\n\nDependency path:%s\n\n"+
			"Consider adding %q to 'apex_available' property of %q",
			fromName, toName, ctx.GetPathString(true), apexName, toName)
		// Visit this module's dependencies to check and report any issues with their availability.
		return true
	})
}

// checkStaticExecutable ensures that executables in an APEX are not static.
func (a *apexBundle) checkStaticExecutables(ctx android.ModuleContext) {
	// No need to run this for host APEXes
	if ctx.Host() {
		return
	}

	ctx.VisitDirectDepsBlueprint(func(module blueprint.Module) {
		if ctx.OtherModuleDependencyTag(module) != executableTag {
			return
		}

		if l, ok := module.(cc.LinkableInterface); ok && l.StaticExecutable() {
			apex := a.ApexVariationName()
			exec := ctx.OtherModuleName(module)
			if isStaticExecutableAllowed(apex, exec) {
				return
			}
			ctx.ModuleErrorf("executable %s is static", ctx.OtherModuleName(module))
		}
	})
}

// A small list of exceptions where static executables are allowed in APEXes.
func isStaticExecutableAllowed(apex string, exec string) bool {
	m := map[string][]string{
		"com.android.runtime": {
			"linker",
			"linkerconfig",
		},
	}
	execNames, ok := m[apex]
	return ok && android.InList(exec, execNames)
}

// Collect information for opening IDE project files in java/jdeps.go.
func (a *apexBundle) IDEInfo(dpInfo *android.IdeInfo) {
	dpInfo.Deps = append(dpInfo.Deps, a.properties.Java_libs...)
	dpInfo.Deps = append(dpInfo.Deps, a.properties.Bootclasspath_fragments...)
	dpInfo.Deps = append(dpInfo.Deps, a.properties.Systemserverclasspath_fragments...)
	dpInfo.Paths = append(dpInfo.Paths, a.modulePaths...)
}

var (
	apexAvailBaseline        = makeApexAvailableBaseline()
	inverseApexAvailBaseline = invertApexBaseline(apexAvailBaseline)
)

func baselineApexAvailable(apex, moduleName string) bool {
	key := apex
	moduleName = normalizeModuleName(moduleName)

	if val, ok := apexAvailBaseline[key]; ok && android.InList(moduleName, val) {
		return true
	}

	key = android.AvailableToAnyApex
	if val, ok := apexAvailBaseline[key]; ok && android.InList(moduleName, val) {
		return true
	}

	return false
}

func normalizeModuleName(moduleName string) string {
	// Prebuilt modules (e.g. java_import, etc.) have "prebuilt_" prefix added by the build
	// system. Trim the prefix for the check since they are confusing
	moduleName = android.RemoveOptionalPrebuiltPrefix(moduleName)
	if strings.HasPrefix(moduleName, "libclang_rt.") {
		// This module has many arch variants that depend on the product being built.
		// We don't want to list them all
		moduleName = "libclang_rt"
	}
	if strings.HasPrefix(moduleName, "androidx.") {
		// TODO(b/156996905) Set apex_available/min_sdk_version for androidx support libraries
		moduleName = "androidx"
	}
	return moduleName
}

// Transform the map of apex -> modules to module -> apexes.
func invertApexBaseline(m map[string][]string) map[string][]string {
	r := make(map[string][]string)
	for apex, modules := range m {
		for _, module := range modules {
			r[module] = append(r[module], apex)
		}
	}
	return r
}

// Retrieve the baseline of apexes to which the supplied module belongs.
func BaselineApexAvailable(moduleName string) []string {
	return inverseApexAvailBaseline[normalizeModuleName(moduleName)]
}

// This is a map from apex to modules, which overrides the apex_available setting for that
// particular module to make it available for the apex regardless of its setting.
// TODO(b/147364041): remove this
func makeApexAvailableBaseline() map[string][]string {
	// The "Module separator"s below are employed to minimize merge conflicts.
	m := make(map[string][]string)
	//
	// Module separator
	//
	m["com.android.appsearch"] = []string{
		"icing-java-proto-lite",
		"libprotobuf-java-lite",
	}
	//
	// Module separator
	//
	m["com.android.btservices"] = []string{
		// empty
	}
	//
	// Module separator
	//
	m["com.android.cellbroadcast"] = []string{"CellBroadcastApp", "CellBroadcastServiceModule"}
	//
	// Module separator
	//
	m["com.android.extservices"] = []string{
		"error_prone_annotations",
		"ExtServices-core",
		"ExtServices",
		"libtextclassifier-java",
		"libz_current",
		"textclassifier-statsd",
		"TextClassifierNotificationLibNoManifest",
		"TextClassifierServiceLibNoManifest",
	}
	//
	// Module separator
	//
	m["com.android.neuralnetworks"] = []string{
		"android.hardware.neuralnetworks@1.0",
		"android.hardware.neuralnetworks@1.1",
		"android.hardware.neuralnetworks@1.2",
		"android.hardware.neuralnetworks@1.3",
		"android.hidl.allocator@1.0",
		"android.hidl.memory.token@1.0",
		"android.hidl.memory@1.0",
		"android.hidl.safe_union@1.0",
		"libarect",
		"libbuildversion",
		"libmath",
		"libprocpartition",
	}
	//
	// Module separator
	//
	m["com.android.media"] = []string{
		// empty
	}
	//
	// Module separator
	//
	m["com.android.media.swcodec"] = []string{
		// empty
	}
	//
	// Module separator
	//
	m["com.android.mediaprovider"] = []string{
		"MediaProvider",
		"MediaProviderGoogle",
		"fmtlib_ndk",
		"libbase_ndk",
		"libfuse",
		"libfuse_jni",
	}
	//
	// Module separator
	//
	m["com.android.runtime"] = []string{
		"bionic_libc_platform_headers",
		"libarm-optimized-routines-math",
		"libc_aeabi",
		"libc_bionic",
		"libc_bionic_ndk",
		"libc_bootstrap",
		"libc_common",
		"libc_common_shared",
		"libc_common_static",
		"libc_dns",
		"libc_dynamic_dispatch",
		"libc_fortify",
		"libc_freebsd",
		"libc_freebsd_large_stack",
		"libc_gdtoa",
		"libc_init_dynamic",
		"libc_init_static",
		"libc_jemalloc_wrapper",
		"libc_netbsd",
		"libc_nomalloc",
		"libc_nopthread",
		"libc_openbsd",
		"libc_openbsd_large_stack",
		"libc_openbsd_ndk",
		"libc_pthread",
		"libc_static_dispatch",
		"libc_syscalls",
		"libc_tzcode",
		"libc_unwind_static",
		"libdebuggerd",
		"libdebuggerd_common_headers",
		"libdebuggerd_handler_core",
		"libdebuggerd_handler_fallback",
		"libdl_static",
		"libjemalloc5",
		"liblinker_main",
		"liblinker_malloc",
		"liblz4",
		"liblzma",
		"libprocinfo",
		"libpropertyinfoparser",
		"libscudo",
		"libstdc++",
		"libsystemproperties",
		"libtombstoned_client_static",
		"libunwindstack",
		"libz",
		"libziparchive",
	}
	//
	// Module separator
	//
	m["com.android.tethering"] = []string{
		"android.hardware.tetheroffload.config-V1.0-java",
		"android.hardware.tetheroffload.control-V1.0-java",
		"android.hidl.base-V1.0-java",
		"libcgrouprc",
		"libcgrouprc_format",
		"libtetherutilsjni",
		"libvndksupport",
		"net-utils-framework-common",
		"netd_aidl_interface-V3-java",
		"netlink-client",
		"networkstack-aidl-interfaces-java",
		"tethering-aidl-interfaces-java",
		"TetheringApiCurrentLib",
	}
	//
	// Module separator
	//
	m["com.android.wifi"] = []string{
		"PlatformProperties",
		"android.hardware.wifi-V1.0-java",
		"android.hardware.wifi-V1.0-java-constants",
		"android.hardware.wifi-V1.1-java",
		"android.hardware.wifi-V1.2-java",
		"android.hardware.wifi-V1.3-java",
		"android.hardware.wifi-V1.4-java",
		"android.hardware.wifi.hostapd-V1.0-java",
		"android.hardware.wifi.hostapd-V1.1-java",
		"android.hardware.wifi.hostapd-V1.2-java",
		"android.hardware.wifi.supplicant-V1.0-java",
		"android.hardware.wifi.supplicant-V1.1-java",
		"android.hardware.wifi.supplicant-V1.2-java",
		"android.hardware.wifi.supplicant-V1.3-java",
		"android.hidl.base-V1.0-java",
		"android.hidl.manager-V1.0-java",
		"android.hidl.manager-V1.1-java",
		"android.hidl.manager-V1.2-java",
		"vendor.qti.hardware.wifi.hostapd-V1.0-java",
		"vendor.qti.hardware.wifi.hostapd-V1.1-java",
		"vendor.qti.hardware.wifi.hostapd-V1.2-java",
		"vendor.qti.hardware.wifi.supplicant-V2.0-java",
		"vendor.qti.hardware.wifi.supplicant-V2.1-java",
		"vendor.qti.hardware.wifi.supplicant-V2.2-java",
		"vendor.qti.hardware.fstman-V1.0-java",
		"bouncycastle-unbundled",
		"dnsresolver_aidl_interface-V2-java",
		"error_prone_annotations",
		"framework-wifi-pre-jarjar",
		"framework-wifi-util-lib",
		"ipmemorystore-aidl-interfaces-V3-java",
		"ipmemorystore-aidl-interfaces-java",
		"ksoap2",
		"libnanohttpd",
		"libwifi-jni",
		"net-utils-services-common",
		"netd_aidl_interface-V2-java",
		"netd_aidl_interface-unstable-java",
		"netd_event_listener_interface-java",
		"netlink-client",
		"networkstack-client",
		"services.net",
		"wifi-lite-protos",
		"wifi-nano-protos",
		"wifi-service-pre-jarjar",
		"wifi-service-resources",
	}
	//
	// Module separator
	//
	m["com.android.os.statsd"] = []string{
		"libstatssocket",
	}
	//
	// Module separator
	//
	m[android.AvailableToAnyApex] = []string{
		// TODO(b/156996905) Set apex_available/min_sdk_version for androidx/extras support libraries
		"androidx",
		"androidx-constraintlayout_constraintlayout",
		"androidx-constraintlayout_constraintlayout-nodeps",
		"androidx-constraintlayout_constraintlayout-solver",
		"androidx-constraintlayout_constraintlayout-solver-nodeps",
		"com.google.android.material_material",
		"com.google.android.material_material-nodeps",

		"libclang_rt",
		"libprofile-clang-extras",
		"libprofile-clang-extras_ndk",
		"libprofile-extras",
		"libprofile-extras_ndk",
		"libunwind",
	}
	return m
}

func init() {
	android.AddNeverAllowRules(createBcpPermittedPackagesRules(qBcpPackages())...)
	android.AddNeverAllowRules(createBcpPermittedPackagesRules(rBcpPackages())...)
}

func createBcpPermittedPackagesRules(bcpPermittedPackages map[string][]string) []android.Rule {
	rules := make([]android.Rule, 0, len(bcpPermittedPackages))
	for jar, permittedPackages := range bcpPermittedPackages {
		permittedPackagesRule := android.NeverAllow().
			With("name", jar).
			WithMatcher("permitted_packages", android.NotInList(permittedPackages)).
			Because(jar +
				" bootjar may only use these package prefixes: " + strings.Join(permittedPackages, ",") +
				". Please consider the following alternatives:\n" +
				"    1. If the offending code is from a statically linked library, consider " +
				"removing that dependency and using an alternative already in the " +
				"bootclasspath, or perhaps a shared library." +
				"    2. Move the offending code into an allowed package.\n" +
				"    3. Jarjar the offending code. Please be mindful of the potential system " +
				"health implications of bundling that code, particularly if the offending jar " +
				"is part of the bootclasspath.")

		rules = append(rules, permittedPackagesRule)
	}
	return rules
}

// DO NOT EDIT! These are the package prefixes that are exempted from being AOT'ed by ART.
// Adding code to the bootclasspath in new packages will cause issues on module update.
func qBcpPackages() map[string][]string {
	return map[string][]string{
		"conscrypt": {
			"android.net.ssl",
			"com.android.org.conscrypt",
		},
		"updatable-media": {
			"android.media",
		},
	}
}

// DO NOT EDIT! These are the package prefixes that are exempted from being AOT'ed by ART.
// Adding code to the bootclasspath in new packages will cause issues on module update.
func rBcpPackages() map[string][]string {
	return map[string][]string{
		"framework-mediaprovider": {
			"android.provider",
		},
		"framework-permission": {
			"android.permission",
			"android.app.role",
			"com.android.permission",
			"com.android.role",
		},
		"framework-sdkextensions": {
			"android.os.ext",
		},
		"framework-statsd": {
			"android.app",
			"android.os",
			"android.util",
			"com.android.internal.statsd",
			"com.android.server.stats",
		},
		"framework-wifi": {
			"com.android.server.wifi",
			"com.android.wifi.x",
			"android.hardware.wifi",
			"android.net.wifi",
		},
		"framework-tethering": {
			"android.net",
		},
	}
}

// For Bazel / bp2build

type bazelApexBundleAttributes struct {
	Manifest              bazel.LabelAttribute
	Android_manifest      bazel.LabelAttribute
	File_contexts         bazel.LabelAttribute
	Canned_fs_config      bazel.LabelAttribute
	Key                   bazel.LabelAttribute
	Certificate           bazel.LabelAttribute  // used when the certificate prop is a module
	Certificate_name      bazel.StringAttribute // used when the certificate prop is a string
	Min_sdk_version       bazel.StringAttribute
	Updatable             bazel.BoolAttribute
	Installable           bazel.BoolAttribute
	Binaries              bazel.LabelListAttribute
	Prebuilts             bazel.LabelListAttribute
	Native_shared_libs_32 bazel.LabelListAttribute
	Native_shared_libs_64 bazel.LabelListAttribute
	Compressible          bazel.BoolAttribute
	Package_name          *string
	Logging_parent        *string
	Tests                 bazel.LabelListAttribute
	Base_apex_name        *string
}

type convertedNativeSharedLibs struct {
	Native_shared_libs_32 bazel.LabelListAttribute
	Native_shared_libs_64 bazel.LabelListAttribute
}

const (
	minSdkVersionPropName = "Min_sdk_version"
)

// ConvertWithBp2build performs bp2build conversion of an apex
func (a *apexBundle) ConvertWithBp2build(ctx android.TopDownMutatorContext) {
	// We only convert apex and apex_test modules at this time
	if ctx.ModuleType() != "apex" && ctx.ModuleType() != "apex_test" {
		return
	}

	attrs, props, commonAttrs := convertWithBp2build(a, ctx)
	commonAttrs.Name = a.Name()
	ctx.CreateBazelTargetModule(props, commonAttrs, &attrs)
}

func convertWithBp2build(a *apexBundle, ctx android.TopDownMutatorContext) (bazelApexBundleAttributes, bazel.BazelTargetModuleProperties, android.CommonAttributes) {
	var manifestLabelAttribute bazel.LabelAttribute
	manifestLabelAttribute.SetValue(android.BazelLabelForModuleSrcSingle(ctx, proptools.StringDefault(a.properties.Manifest, "apex_manifest.json")))

	var androidManifestLabelAttribute bazel.LabelAttribute
	if a.properties.AndroidManifest != nil {
		androidManifestLabelAttribute.SetValue(android.BazelLabelForModuleSrcSingle(ctx, *a.properties.AndroidManifest))
	}

	var fileContextsLabelAttribute bazel.LabelAttribute
	if a.properties.File_contexts == nil {
		// See buildFileContexts(), if file_contexts is not specified the default one is used, which is //system/sepolicy/apex:<module name>-file_contexts
		fileContextsLabelAttribute.SetValue(android.BazelLabelForModuleDepSingle(ctx, a.Name()+"-file_contexts"))
	} else if strings.HasPrefix(*a.properties.File_contexts, ":") {
		// File_contexts is a module
		fileContextsLabelAttribute.SetValue(android.BazelLabelForModuleDepSingle(ctx, *a.properties.File_contexts))
	} else {
		// File_contexts is a file
		fileContextsLabelAttribute.SetValue(android.BazelLabelForModuleSrcSingle(ctx, *a.properties.File_contexts))
	}

	var cannedFsConfigAttribute bazel.LabelAttribute
	if a.properties.Canned_fs_config != nil {
		cannedFsConfigAttribute.SetValue(android.BazelLabelForModuleSrcSingle(ctx, *a.properties.Canned_fs_config))
	}

	productVariableProps := android.ProductVariableProperties(ctx, a)
	// TODO(b/219503907) this would need to be set to a.MinSdkVersionValue(ctx) but
	// given it's coming via config, we probably don't want to put it in here.
	var minSdkVersion bazel.StringAttribute
	if a.properties.Min_sdk_version != nil {
		minSdkVersion.SetValue(*a.properties.Min_sdk_version)
	}
	if props, ok := productVariableProps[minSdkVersionPropName]; ok {
		for c, p := range props {
			if val, ok := p.(*string); ok {
				minSdkVersion.SetSelectValue(c.ConfigurationAxis(), c.SelectKey(), val)
			}
		}
	}

	var keyLabelAttribute bazel.LabelAttribute
	if a.overridableProperties.Key != nil {
		keyLabelAttribute.SetValue(android.BazelLabelForModuleDepSingle(ctx, *a.overridableProperties.Key))
	}

	// Certificate
	certificate, certificateName := android.BazelStringOrLabelFromProp(ctx, a.overridableProperties.Certificate)

	nativeSharedLibs := &convertedNativeSharedLibs{
		Native_shared_libs_32: bazel.LabelListAttribute{},
		Native_shared_libs_64: bazel.LabelListAttribute{},
	}

	// https://cs.android.com/android/platform/superproject/+/master:build/soong/android/arch.go;l=698;drc=f05b0d35d2fbe51be9961ce8ce8031f840295c68
	// https://cs.android.com/android/platform/superproject/+/master:build/soong/apex/apex.go;l=2549;drc=ec731a83e3e2d80a1254e32fd4ad7ef85e262669
	// In Soong, decodeMultilib, used to get multilib, return "first" if defaultMultilib is set to "common".
	// Since apex sets defaultMultilib to be "common", equivalent compileMultilib in bp2build for apex should be "first"
	compileMultilib := "first"
	if a.CompileMultilib() != nil {
		compileMultilib = *a.CompileMultilib()
	}

	// properties.Native_shared_libs is treated as "both"
	convertBothLibs(ctx, compileMultilib, a.properties.Native_shared_libs, nativeSharedLibs)
	convertBothLibs(ctx, compileMultilib, a.properties.Multilib.Both.Native_shared_libs, nativeSharedLibs)
	convert32Libs(ctx, compileMultilib, a.properties.Multilib.Lib32.Native_shared_libs, nativeSharedLibs)
	convert64Libs(ctx, compileMultilib, a.properties.Multilib.Lib64.Native_shared_libs, nativeSharedLibs)
	convertFirstLibs(ctx, compileMultilib, a.properties.Multilib.First.Native_shared_libs, nativeSharedLibs)

	prebuilts := a.overridableProperties.Prebuilts
	prebuiltsLabelList := android.BazelLabelForModuleDeps(ctx, prebuilts)
	prebuiltsLabelListAttribute := bazel.MakeLabelListAttribute(prebuiltsLabelList)

	binaries := android.BazelLabelForModuleDeps(ctx, a.properties.ApexNativeDependencies.Binaries)
	binariesLabelListAttribute := bazel.MakeLabelListAttribute(binaries)

	var testsAttrs bazel.LabelListAttribute
	if a.testApex && len(a.properties.ApexNativeDependencies.Tests) > 0 {
		tests := android.BazelLabelForModuleDeps(ctx, a.properties.ApexNativeDependencies.Tests)
		testsAttrs = bazel.MakeLabelListAttribute(tests)
	}

	var updatableAttribute bazel.BoolAttribute
	if a.properties.Updatable != nil {
		updatableAttribute.Value = a.properties.Updatable
	}

	var installableAttribute bazel.BoolAttribute
	if a.properties.Installable != nil {
		installableAttribute.Value = a.properties.Installable
	}

	var compressibleAttribute bazel.BoolAttribute
	if a.overridableProperties.Compressible != nil {
		compressibleAttribute.Value = a.overridableProperties.Compressible
	}

	var packageName *string
	if a.overridableProperties.Package_name != "" {
		packageName = &a.overridableProperties.Package_name
	}

	var loggingParent *string
	if a.overridableProperties.Logging_parent != "" {
		loggingParent = &a.overridableProperties.Logging_parent
	}

	attrs := bazelApexBundleAttributes{
		Manifest:              manifestLabelAttribute,
		Android_manifest:      androidManifestLabelAttribute,
		File_contexts:         fileContextsLabelAttribute,
		Canned_fs_config:      cannedFsConfigAttribute,
		Min_sdk_version:       minSdkVersion,
		Key:                   keyLabelAttribute,
		Certificate:           certificate,
		Certificate_name:      certificateName,
		Updatable:             updatableAttribute,
		Installable:           installableAttribute,
		Native_shared_libs_32: nativeSharedLibs.Native_shared_libs_32,
		Native_shared_libs_64: nativeSharedLibs.Native_shared_libs_64,
		Binaries:              binariesLabelListAttribute,
		Prebuilts:             prebuiltsLabelListAttribute,
		Compressible:          compressibleAttribute,
		Package_name:          packageName,
		Logging_parent:        loggingParent,
		Tests:                 testsAttrs,
	}

	props := bazel.BazelTargetModuleProperties{
		Rule_class:        "apex",
		Bzl_load_location: "//build/bazel/rules/apex:apex.bzl",
	}

	commonAttrs := android.CommonAttributes{}
	if a.testApex {
		commonAttrs.Testonly = proptools.BoolPtr(true)
	}

	return attrs, props, commonAttrs
}

// The following conversions are based on this table where the rows are the compile_multilib
// values and the columns are the properties.Multilib.*.Native_shared_libs. Each cell
// represents how the libs should be compiled for a 64-bit/32-bit device: 32 means it
// should be compiled as 32-bit, 64 means it should be compiled as 64-bit, none means it
// should not be compiled.
// multib/compile_multilib, 32,        64,        both,     first
// 32,                      32/32,     none/none, 32/32,    none/32
// 64,                      none/none, 64/none,   64/none,  64/none
// both,                    32/32,     64/none,   32&64/32, 64/32
// first,                   32/32,     64/none,   64/32,    64/32

func convert32Libs(ctx android.TopDownMutatorContext, compileMultilb string,
	libs []string, nativeSharedLibs *convertedNativeSharedLibs) {
	libsLabelList := android.BazelLabelForModuleDeps(ctx, libs)
	switch compileMultilb {
	case "both", "32":
		makeNoConfig32SharedLibsAttributes(libsLabelList, nativeSharedLibs)
	case "first":
		make32SharedLibsAttributes(libsLabelList, nativeSharedLibs)
	case "64":
		// Incompatible, ignore
	default:
		invalidCompileMultilib(ctx, compileMultilb)
	}
}

func convert64Libs(ctx android.TopDownMutatorContext, compileMultilb string,
	libs []string, nativeSharedLibs *convertedNativeSharedLibs) {
	libsLabelList := android.BazelLabelForModuleDeps(ctx, libs)
	switch compileMultilb {
	case "both", "64", "first":
		make64SharedLibsAttributes(libsLabelList, nativeSharedLibs)
	case "32":
		// Incompatible, ignore
	default:
		invalidCompileMultilib(ctx, compileMultilb)
	}
}

func convertBothLibs(ctx android.TopDownMutatorContext, compileMultilb string,
	libs []string, nativeSharedLibs *convertedNativeSharedLibs) {
	libsLabelList := android.BazelLabelForModuleDeps(ctx, libs)
	switch compileMultilb {
	case "both":
		makeNoConfig32SharedLibsAttributes(libsLabelList, nativeSharedLibs)
		make64SharedLibsAttributes(libsLabelList, nativeSharedLibs)
	case "first":
		makeFirstSharedLibsAttributes(libsLabelList, nativeSharedLibs)
	case "32":
		makeNoConfig32SharedLibsAttributes(libsLabelList, nativeSharedLibs)
	case "64":
		make64SharedLibsAttributes(libsLabelList, nativeSharedLibs)
	default:
		invalidCompileMultilib(ctx, compileMultilb)
	}
}

func convertFirstLibs(ctx android.TopDownMutatorContext, compileMultilb string,
	libs []string, nativeSharedLibs *convertedNativeSharedLibs) {
	libsLabelList := android.BazelLabelForModuleDeps(ctx, libs)
	switch compileMultilb {
	case "both", "first":
		makeFirstSharedLibsAttributes(libsLabelList, nativeSharedLibs)
	case "32":
		make32SharedLibsAttributes(libsLabelList, nativeSharedLibs)
	case "64":
		make64SharedLibsAttributes(libsLabelList, nativeSharedLibs)
	default:
		invalidCompileMultilib(ctx, compileMultilb)
	}
}

func makeFirstSharedLibsAttributes(libsLabelList bazel.LabelList, nativeSharedLibs *convertedNativeSharedLibs) {
	make32SharedLibsAttributes(libsLabelList, nativeSharedLibs)
	make64SharedLibsAttributes(libsLabelList, nativeSharedLibs)
}

func makeNoConfig32SharedLibsAttributes(libsLabelList bazel.LabelList, nativeSharedLibs *convertedNativeSharedLibs) {
	list := bazel.LabelListAttribute{}
	list.SetSelectValue(bazel.NoConfigAxis, "", libsLabelList)
	nativeSharedLibs.Native_shared_libs_32.Append(list)
}

func make32SharedLibsAttributes(libsLabelList bazel.LabelList, nativeSharedLibs *convertedNativeSharedLibs) {
	makeSharedLibsAttributes("x86", libsLabelList, &nativeSharedLibs.Native_shared_libs_32)
	makeSharedLibsAttributes("arm", libsLabelList, &nativeSharedLibs.Native_shared_libs_32)
}

func make64SharedLibsAttributes(libsLabelList bazel.LabelList, nativeSharedLibs *convertedNativeSharedLibs) {
	makeSharedLibsAttributes("x86_64", libsLabelList, &nativeSharedLibs.Native_shared_libs_64)
	makeSharedLibsAttributes("arm64", libsLabelList, &nativeSharedLibs.Native_shared_libs_64)
}

func makeSharedLibsAttributes(config string, libsLabelList bazel.LabelList,
	labelListAttr *bazel.LabelListAttribute) {
	list := bazel.LabelListAttribute{}
	list.SetSelectValue(bazel.ArchConfigurationAxis, config, libsLabelList)
	labelListAttr.Append(list)
}

func invalidCompileMultilib(ctx android.TopDownMutatorContext, value string) {
	ctx.PropertyErrorf("compile_multilib", "Invalid value: %s", value)
}
