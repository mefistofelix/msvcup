# Engineering guidelines

## Project constraints

- Keep application code in exactly `src/msvcup.go` and `src/archives.go`.
- `msvcup.go` owns CLI and domain logic. `archives.go` owns generic archive/container handling.
- Translate Visual Studio `ExtensionDir`, `ArchivePrefix`, and `ArchiveTarget` metadata in `msvcup.go`; pass generic destinations and prefixes to `archives.go`.
- Build with `CGO_ENABLED=0` for Windows amd64 and Linux amd64.
- Keep build orchestration in `build.sh`. The GitHub workflow must invoke it rather than duplicate its toolchain or compiler commands.
- Linux lists, resolves, downloads, and extracts Windows toolchains. Do not generate Bash environment scripts or try to execute Microsoft binaries on Linux.
- Never invoke `msiexec`, Wine, 7-Zip, downloaded installers, or platform-specific extraction DLLs.
- Do not add WiX Burn bootstrapper parsing to the SDK/WDK resolution path.
- Use only the currently approved archive dependencies. Do not add a dependency without explicit approval.

## Package sources and layout

- Resolve MSVC from direct VSIX packages in the selected Visual Studio channel. Do not select the complete Visual Studio C++ component for the `msvc` alias.
- Resolve the native Windows SDK and WDK from the official `WindowsSDK` NuGet profile.
- `Microsoft.Windows.SDK.CPP/c` and `Microsoft.Windows.WDK.<arch>/c` map to `Windows Kits/10`.
- `Microsoft.Windows.SDK.CPP.<arch>/c` maps to `Windows Kits/10/Lib/<SDK ABI version>`.
- Treat these as package-root mappings. Do not add individual file relocation rules.
- Keep SDK and WDK NuGet package versions identical. The WDK architecture package has a required dependency on the corresponding SDK architecture package, which depends on the common SDK package.
- The common C++ SDK package already includes native SDK tools such as `rc.exe` and `midl.exe`; do not also install `Microsoft.Windows.SDK.BuildTools`.

## Simplicity and readability

- Optimize for cognitive simplicity first and line count second.
- Let each line express one clear operation. Do not compress unrelated operations.
- Prefer guard clauses so the main path stays flat.
- Use descriptive names. Keep idiomatic short names only when their meaning is immediate, such as `err` or a small method receiver.
- Keep architecture names, aliases, channel families, and standard `vcvars*.bat` variants in their declarative tables instead of adding parallel switches.
- Prefer standard-library and language features over wrappers.
- Add helpers and abstractions only for a concrete repeated operation or a strict layer boundary.
- Keep format handling generic. Driver-, SDK-, and component-specific choices belong in the CLI/domain layer.

## Errors, integrity, and concurrency

- Propagate native errors unless added context identifies the failed package, payload, or boundary.
- Never silently ignore required dependencies, corrupt archives, unsafe paths, or failed hash checks.
- Payload mismatches may be accepted only through the explicit install option and must emit the observed hash.
- Resolve all wildcard exclusions before creating a destination or downloading payloads.
- Use concurrency only for independent downloads. Keep filesystem installation deterministic and sequential.

## Verification

Run before committing:

```sh
./build.sh
```

Format `src/msvcup.go` and `src/archives.go` with `gofmt` before running the build script.

For changes affecting installation or `vcvars*.bat`, perform the real Windows validation:

1. Install downloaded MSVC, SDK, and WDK content into an empty directory.
2. Enter the environment through a generated `vcvars*.bat` file.
3. Compile, link, and run a C hello world.
4. Compile and link a kernel hello driver and verify it is an x64 PE with Native subsystem.
