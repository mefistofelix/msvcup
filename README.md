# msvcup

`msvcup` downloads and expands portable MSVC, Windows SDK, and WDK toolchains without installing Visual Studio or invoking `msiexec`, Wine, 7-Zip, cgo, or registry APIs.

The executable runs on Windows and Linux. Linux can list, download, and extract the Windows toolchain; the downloaded Microsoft programs still run only on Windows.

## Quick start

```bat
msvcup list "*DriverKit*"
msvcup install "msvc sdk wdk" toolchain
call toolchain\vcvars64.bat
cl hello.c
```

The defaults are Visual Studio 18, the stable channel, and the current architecture. Select another family with `--vs`, use `--channel preview`, or pass an explicit HTTP/HTTPS channel URL to `--channel`. `--target` accepts a comma-separated architecture list.

MSVC comes from the selected Visual Studio catalog. `sdk` and `wdk` use the official Microsoft NuGet packages. Their package roots are merged as declared by Microsoft: common SDK and WDK content maps to `Windows Kits/10`, while target-specific SDK libraries map to `Windows Kits/10/Lib/<version>`. Files are not relocated individually.

## Selection

The package list is one quoted argument containing space-separated selectors. Selectors support case-insensitive `*`, `?`, and `[...]` wildcards. Prefix a selector with `-` to exclude it:

```bat
msvcup list "*DriverKit* -*.Resources.*"
msvcup install "msvc sdk wdk -*.ARM64*" toolchain
```

Excluding a directly selected package skips it. Excluding a required dependency aborts before downloads or destination changes and prints the dependency chain.

## Lock and downloads

An install writes `DIR.lock` next to the destination. It records the selection, architectures, channel, package versions, payload URLs, sizes, and hashes. A later invocation with the same arguments reads the lock without querying Microsoft manifests. Use `--update-lock` to resolve and replace it.

There is no persistent payload cache. Every installation downloads into a temporary directory and removes it when finished, so a separate force-download mode is unnecessary.

Microsoft currently serves some re-signed VSIX payloads whose bytes no longer match its catalog. These mismatches are accepted with a warning by default. Use `--allow-payload-mismatch=false` for strict verification. NuGet SDK and WDK payloads are always verified with the official SHA-512.

## Visual Studio command prompts

The destination contains `vcvarsall.bat` and the applicable standard Microsoft files, such as `vcvars64.bat`, `vcvars32.bat`, or `vcvarsamd64_arm64.bat`. They are rendered with Go's standard `text/template` package. No project-specific aliases or shell environment scripts are generated.

## Archive support

- VSIX, ZIP, and NUPKG through Go's standard ZIP reader.
- MSI relational tables and embedded/external CAB discovery in pure Go.
- CAB uncompressed and MSZIP data in pure Go.

CAB LZX and Quantum are rejected explicitly because the available pure-Go backend does not support them. The normal SDK/WDK path uses NUPKG and does not parse or execute WiX Burn bootstrapper executables.

## Build

Application code is split into exactly two Go files:

- `src/msvcup.go`: CLI, catalogs, resolution, download, lock, and environment scripts.
- `src/archives.go`: generic ZIP/VSIX/NUPKG, MSI/CFB, and CAB extraction.

```sh
./build.sh
```

`build.sh` downloads Go 1.26.5 under `build/golang`, runs tests and vet, then builds `bin/msvcup` and `bin/msvcup.exe` with cgo disabled. The GitHub workflow uses no actions: it checks out the requested commit with Git, invokes this script on Linux, and publishes those two files.
