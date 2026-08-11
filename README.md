# msvcup

`msvcup` downloads and expands portable MSVC, Windows SDK, and WDK toolchains without installing Visual Studio or invoking `msiexec`, Wine, 7-Zip, cgo, or registry APIs.

The executable runs on Windows and Linux. Linux support is limited to listing, resolving, downloading, and extracting Windows toolchains; the downloaded Microsoft tools run on Windows.

## Quick start

```bat
msvcup list --details "*DriverKit*"
msvcup resolve "msvc sdk wdk"
msvcup install "msvc sdk wdk" toolchain
call toolchain\vcvars64.bat
cl hello.c
```

The default channel is `latest/stable`. Select another family or channel with `--vs 2022`, `--vs 2026`, or `--channel insiders`. `--channel` also accepts an explicit HTTP or HTTPS channel manifest URL. `--target` defaults to the architecture on which `msvcup` was built.

MSVC comes from the Visual Studio release catalog. `sdk` resolves the official `Microsoft.Windows.SDK.CPP` package and the package for each selected target architecture. `wdk` adds the matching `Microsoft.Windows.WDK.<arch>` package and the Visual Studio WDK build integration. WDK NuGet supports x64 and ARM64 targets.

The Windows Kit packages are merged according to the roots declared by Microsoft: the common SDK and WDK roots become `Windows Kits/10`, while architecture-specific SDK libraries become `Windows Kits/10/Lib/<version>`. Package contents are not relocated file by file.

## Selection and exclusions

Component arguments accept case-insensitive `*`, `?`, and `[...]` wildcards. Prefix a pattern with `-` to exclude matches:

```bat
msvcup resolve "Component.Microsoft.Windows.DriverKit* -Component.Microsoft.Windows.DriverKit"
```

The package selection is one quoted argument containing space-separated selectors. Excluding a directly selected package skips it. Excluding a `Required` dependency aborts resolution before downloads or destination changes and prints the dependency chain.

`list` uses the same single selection argument and wildcard/exclusion rules:

```bat
msvcup list "*DriverKit* -*.Resources.*"
msvcup list "sdk"
msvcup list --type all "*Windows*SDK*"
```

## Lock and cache

An install writes `DIR.lock` next to the destination. The JSON lock contains the roots, target architectures, package versions, payload URLs, sizes, and hashes. A later invocation with the same arguments uses the lock without querying Microsoft manifests. Use `--update-lock` to resolve and replace it or `--lock-file PATH` to choose another location.

Downloads use the global user cache by default:

- Windows: `%LOCALAPPDATA%\msvcup\cache`
- Linux: `$XDG_CACHE_HOME/msvcup/cache` or `$HOME/.cache/msvcup/cache`

Override it with `--cache DIR`.

Microsoft currently serves some re-signed VSIX payloads whose bytes no longer match its own catalog. Mismatches are accepted with a warning by default. Use `--allow-payload-mismatch=false` for strict size and hash verification. NuGet SDK and WDK payloads are always verified with their catalog SHA-512.

## Visual Studio command prompts

The destination contains `vcvarsall.bat` and the applicable standard Microsoft command files, such as `vcvars64.bat`, `vcvars32.bat`, or `vcvarsamd64_arm64.bat`. It does not create project-specific aliases.

No shell environment scripts are generated.

## Archive support

- VSIX, ZIP, and NUPKG through Go's standard ZIP reader.
- MSI relational tables and embedded/external CAB discovery in pure Go.
- CAB uncompressed and MSZIP data in pure Go.

CAB LZX and Quantum are rejected explicitly because the available WIM LZX decoder is not compatible with CAB LZX.

The normal SDK/WDK path uses NUPKG archives and does not parse or execute WiX Burn bootstrapper executables.

## Build

The application source is split into exactly two Go files:

- `src/msvcup.go`: CLI, catalogs, resolution, cache, lock, and environment scripts.
- `src/archives.go`: ZIP/VSIX/NUPKG, MSI/CFB, and CAB extraction.

```sh
./build.sh
```

`build.sh` downloads and caches Go 1.26.5 under `build/golang`, runs tests and vet, then builds both executables with cgo disabled. The GitHub workflow checks out the commit without actions, invokes this script on a Linux runner, and publishes its `bin` outputs.
