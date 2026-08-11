// msvcup downloads and expands portable Visual Studio toolchain components.
// It is intentionally pure Go: it never invokes msiexec and uses no cgo.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const version = "0.1.0"

type stringList []string

func (list *stringList) String() string {
	return strings.Join(*list, ",")
}

func (list *stringList) Set(value string) error {
	*list = append(*list, value)
	return nil
}

type payload struct {
	FileName string `json:"fileName"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	SHA512   string `json:"sha512,omitempty"`
	Size     int64  `json:"size"`
}

type localizedResource struct {
	Language    string   `json:"language"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	License     string   `json:"license"`
	Keywords    []string `json:"keywords"`
}

type dependency struct {
	Version     string `json:"version"`
	Type        string `json:"type"`
	Chip        string `json:"chip"`
	ProductArch string `json:"productArch"`
	MachineArch string `json:"machineArch"`
	Language    string `json:"language"`
	Behaviors   string `json:"behaviors"`
}

func (d *dependency) UnmarshalJSON(data []byte) error {
	var version string
	if err := json.Unmarshal(data, &version); err == nil {
		d.Version = version
		return nil
	}
	type plain dependency
	return json.Unmarshal(data, (*plain)(d))
}

type vsPackage struct {
	ID                 string                `json:"id"`
	Version            string                `json:"version"`
	Type               string                `json:"type"`
	Chip               string                `json:"chip"`
	Language           string                `json:"language"`
	MachineArch        string                `json:"machineArch"`
	ProductArch        string                `json:"productArch"`
	ExtensionDir       string                `json:"extensionDir"`
	ArchivePrefix      string                `json:"archivePrefix,omitempty"`
	ArchiveTarget      string                `json:"archiveTarget,omitempty"`
	Payloads           []payload             `json:"payloads"`
	Dependencies       map[string]dependency `json:"dependencies"`
	LocalizedResources []localizedResource   `json:"localizedResources"`
	InstallSizes       map[string]int64      `json:"installSizes"`
}

type vsManifest struct {
	ManifestVersion string      `json:"manifestVersion"`
	EngineVersion   string      `json:"engineVersion"`
	Packages        []vsPackage `json:"packages"`
}

type channelItem struct {
	ID                 string              `json:"id"`
	Version            string              `json:"version"`
	Type               string              `json:"type"`
	Payloads           []payload           `json:"payloads"`
	LocalizedResources []localizedResource `json:"localizedResources"`
}

type channelManifest struct {
	ManifestVersion string        `json:"manifestVersion"`
	ChannelItems    []channelItem `json:"channelItems"`
}

type catalog struct {
	Manifest   *vsManifest
	ChannelURL string
	LicenseURL string
	Version    string
}

type manifestOptions struct {
	VS       string
	Channel  string
	Manifest string
	Language string
	Host     string
}

func addManifestFlags(fs *flag.FlagSet, o *manifestOptions) {
	fs.StringVar(&o.VS, "vs", "latest", "Visual Studio channel family: latest, 2026, 2022, 2019")
	fs.StringVar(&o.Channel, "channel", "stable", "channel selector or explicit http(s) channel manifest URL")
	fs.StringVar(&o.Manifest, "manifest", "", "read a local or remote VisualStudio.vsman directly")
	fs.StringVar(&o.Language, "lang", "en-us", "localized metadata and payload language")
	fs.StringVar(&o.Host, "host", defaultHost(), "host architecture: x64, x86, arm64")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "msvcup:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return flag.ErrHelp
	}
	switch args[0] {
	case "help", "-h", "--help":
		usage()
		return nil
	case "version", "--version":
		fmt.Printf("msvcup %s (%s/%s, pure Go)\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	case "list":
		return runList(args[1:])
	case "resolve":
		return runResolve(args[1:])
	case "install":
		return runInstall(args[1:])
	case "extract-msi":
		return runExtractMSI(args[1:])
	default:
		return fmt.Errorf("unknown command %q (use 'msvcup help')", args[0])
	}
}

func usage() {
	fmt.Print(`msvcup - portable Microsoft C/C++ toolchain downloader

Usage:
	msvcup list [options] ["PACKAGE..."]
  msvcup resolve [options] ["PACKAGE..."]
  msvcup install [options] ["PACKAGE..."] DIR
  msvcup extract-msi [options] MSI DIR

Aliases accepted in PACKAGE:
  msvc       MSVC tools for the selected --target architectures
  sdk        official Microsoft Windows SDK NuGet packages
  wdk        SDK + WDK build integration + official Microsoft WDK NuGet
  wdk-vs     Component.Microsoft.Windows.DriverKit
  vctools    Microsoft.VisualStudio.Workload.VCTools

PACKAGE contains space-separated selectors. Each selector accepts
case-insensitive wildcards (*, ?, [...]). Prefix a selector
with - to exclude it. Excluding a Required dependency aborts resolution before
any download or destination change. The default release channel is stable.

Examples:
	msvcup list "*DriverKit* -*.Resources.*"
	msvcup list --type all "*Windows*SDK*"
  msvcup resolve "Component.Microsoft.Windows.DriverKit* -Component.Microsoft.Windows.DriverKit"
  msvcup resolve --include-recommended "msvc sdk wdk"
  msvcup install --target x64,arm64 "msvc sdk" ./toolchain
  msvcup install "Component.Microsoft.Windows.DriverKit.BuildTools" ./wdk

Options must precede positional arguments. No command invokes installers,
msiexec, Wine, 7-Zip, or platform DLLs.
`)
}

type architectureNames struct {
	MSVC string
	SDK  string
	WDK  string
	Host bool
}

var architectureAliases = map[string]string{
	"amd64":   "x64",
	"x86_64":  "x64",
	"386":     "x86",
	"i386":    "x86",
	"i686":    "x86",
	"win32":   "x86",
	"aarch64": "arm64",
	"neutral": "",
	"any":     "",
}

var architectures = map[string]architectureNames{
	"x64":     {MSVC: "X64", SDK: "x64", WDK: "x64", Host: true},
	"x86":     {MSVC: "X86", SDK: "x86", Host: true},
	"arm":     {MSVC: "ARM", SDK: "ARM"},
	"arm64":   {MSVC: "ARM64", SDK: "ARM64", WDK: "ARM64", Host: true},
	"arm64ec": {MSVC: "ARM64EC"},
}

func defaultHost() string {
	return normalizeArch(runtime.GOARCH)
}

func normalizeArch(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if normalized, found := architectureAliases[value]; found {
		return normalized
	}
	return value
}

type channelPair struct {
	Stable  string
	Preview string
}

var visualStudioFamilies = map[string]string{
	"18": "2026",
	"17": "2022",
	"16": "2019",
}

var visualStudioChannels = map[string]channelPair{
	"latest": {Stable: "https://aka.ms/vs/stable/channel", Preview: "https://aka.ms/vs/insiders/channel"},
	"2026":   {Stable: "https://aka.ms/vs/18/stable/channel", Preview: "https://aka.ms/vs/18/insiders/channel"},
	"2022":   {Stable: "https://aka.ms/vs/17/release/channel", Preview: "https://aka.ms/vs/17/pre/channel"},
	"2019":   {Stable: "https://aka.ms/vs/16/release/channel", Preview: "https://aka.ms/vs/16/pre/channel"},
}

func channelURL(vs, channel string) (string, error) {
	channel = strings.ToLower(channel)
	preview := channel == "insiders" || channel == "preview"
	stable := channel == "stable" || channel == "latest" || channel == "release"
	if !preview && !stable {
		return "", fmt.Errorf("invalid channel %q", channel)
	}
	family := strings.ToLower(vs)
	if alias := visualStudioFamilies[family]; alias != "" {
		family = alias
	}
	channels, found := visualStudioChannels[family]
	if !found {
		return "", fmt.Errorf("invalid Visual Studio family %q", vs)
	}
	if preview {
		return channels.Preview, nil
	}
	return channels.Stable, nil
}

func newHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 16
	transport.MaxIdleConnsPerHost = 8
	transport.ResponseHeaderTimeout = 45 * time.Second
	return &http.Client{Transport: transport, Timeout: timeout}
}

func readResource(client *http.Client, name string, limit int64) ([]byte, error) {
	if u, err := url.Parse(name); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		req, err := http.NewRequest(http.MethodGet, name, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "msvcup/"+version)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", name, resp.Status)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > limit {
			return nil, fmt.Errorf("resource %s exceeds %d bytes", name, limit)
		}
		return data, nil
	}
	return os.ReadFile(name)
}

func loadCatalog(o manifestOptions) (*catalog, error) {
	client := newHTTPClient(3 * time.Minute)
	if o.Manifest != "" {
		data, err := readResource(client, o.Manifest, 512<<20)
		if err != nil {
			return nil, fmt.Errorf("read manifest: %w", err)
		}
		var manifest vsManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("decode manifest: %w", err)
		}
		return &catalog{Manifest: &manifest, Version: manifest.EngineVersion}, nil
	}
	chURL := o.Channel
	parsedChannelURL, parseErr := url.Parse(chURL)
	explicitChannelURL := parseErr == nil && (parsedChannelURL.Scheme == "http" || parsedChannelURL.Scheme == "https")
	if !explicitChannelURL {
		var err error
		chURL, err = channelURL(o.VS, o.Channel)
		if err != nil {
			return nil, err
		}
	}
	data, err := readResource(client, chURL, 32<<20)
	if err != nil {
		return nil, fmt.Errorf("read channel %s: %w", chURL, err)
	}
	var channel channelManifest
	if err := json.Unmarshal(data, &channel); err != nil {
		return nil, fmt.Errorf("decode channel: %w", err)
	}
	manifestID := "Microsoft.VisualStudio.Manifests.VisualStudio"
	previewManifestID := manifestID + "Preview"
	previewChannel := strings.EqualFold(o.Channel, "insiders") || strings.EqualFold(o.Channel, "preview")
	var manifestURL, license, catalogVersion string
	for _, item := range channel.ChannelItems {
		isManifest := strings.EqualFold(item.ID, manifestID)
		if previewChannel {
			isManifest = strings.EqualFold(item.ID, previewManifestID)
		} else if explicitChannelURL {
			isManifest = isManifest || strings.EqualFold(item.ID, previewManifestID)
		}
		if isManifest && manifestURL == "" && len(item.Payloads) > 0 {
			manifestURL = item.Payloads[0].URL
			catalogVersion = item.Version
		}
		if strings.EqualFold(item.ID, "Microsoft.VisualStudio.Product.BuildTools") {
			license = resourceFor(item.LocalizedResources, o.Language).License
		}
	}
	if manifestURL == "" {
		return nil, errors.New("channel has no Visual Studio manifest item")
	}
	data, err = readResource(client, manifestURL, 512<<20)
	if err != nil {
		return nil, fmt.Errorf("read Visual Studio catalog: %w", err)
	}
	var manifest vsManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode Visual Studio catalog: %w", err)
	}
	return &catalog{Manifest: &manifest, ChannelURL: chURL, LicenseURL: license, Version: catalogVersion}, nil
}

type nugetVersionIndex struct {
	Versions []string `json:"versions"`
}

type nugetRegistrationLeaf struct {
	CatalogEntry   json.RawMessage `json:"catalogEntry"`
	PackageContent string          `json:"packageContent"`
}

type nugetCatalogEntry struct {
	PackageHash          string `json:"packageHash"`
	PackageHashAlgorithm string `json:"packageHashAlgorithm"`
	PackageSize          int64  `json:"packageSize"`
	Title                string `json:"title"`
	Description          string `json:"description"`
}

type nugetPackageRequest struct {
	ID               string
	SDKBuild         string
	Channel          string
	PreferredVersion string
	ArchiveTarget    string
}

func addWindowsKitNuGetPackages(cat *catalog, options resolveOptions, roots []string) error {
	needsSDK, needsWDK := requestedWindowsKitPackages(roots, options.Targets)
	if !needsSDK && !needsWDK {
		return nil
	}
	sdkBuild := latestSDKBuild(cat.Manifest.Packages)
	if sdkBuild == "" {
		return errors.New("Windows Kit NuGet packages require a numeric Windows SDK component")
	}
	kitRoot := path.Join("Windows Kits", "10")
	libraryRoot := path.Join(kitRoot, "Lib", "10.0."+sdkBuild+".0")
	version := ""
	var packages []vsPackage
	for _, target := range options.Targets {
		sdkPackageID, err := sdkNuGetPackageID(target)
		if err != nil {
			return err
		}
		if needsWDK {
			wdkPackageID, err := wdkNuGetPackageID(target)
			if err != nil {
				return err
			}
			wdkPackage, err := loadWindowsKitPackage(nugetPackageRequest{
				ID:            wdkPackageID,
				SDKBuild:      sdkBuild,
				Channel:       options.Manifest.Channel,
				ArchiveTarget: kitRoot,
			}, &version)
			if err != nil {
				return err
			}
			wdkPackage.Dependencies = map[string]dependency{
				sdkPackageID: {Version: version, Type: "Required"},
			}
			packages = append(packages, wdkPackage)
		}
		sdkPackage, err := loadWindowsKitPackage(nugetPackageRequest{
			ID:            sdkPackageID,
			SDKBuild:      sdkBuild,
			Channel:       options.Manifest.Channel,
			ArchiveTarget: libraryRoot,
		}, &version)
		if err != nil {
			return err
		}
		sdkPackage.Dependencies = map[string]dependency{
			"Microsoft.Windows.SDK.cpp": {Version: version, Type: "Required"},
		}
		packages = append(packages, sdkPackage)
	}
	commonSDK, err := loadWindowsKitPackage(nugetPackageRequest{
		ID:            "Microsoft.Windows.SDK.cpp",
		SDKBuild:      sdkBuild,
		Channel:       options.Manifest.Channel,
		ArchiveTarget: kitRoot,
	}, &version)
	if err != nil {
		return err
	}
	cat.Manifest.Packages = append(cat.Manifest.Packages, commonSDK)
	cat.Manifest.Packages = append(cat.Manifest.Packages, packages...)
	return nil
}

func requestedWindowsKitPackages(roots, targets []string) (bool, bool) {
	knownIDs := []string{"Microsoft.Windows.SDK.cpp"}
	for _, target := range targets {
		names := architectures[normalizeArch(target)]
		if names.SDK != "" {
			knownIDs = append(knownIDs, "Microsoft.Windows.SDK.cpp."+names.SDK)
		}
		if names.WDK != "" {
			knownIDs = append(knownIDs, "Microsoft.Windows.WDK."+names.WDK)
		}
	}
	needsSDK := false
	needsWDK := false
	for _, root := range roots {
		pattern, excluded := splitPackageSelector(root)
		if excluded {
			continue
		}
		switch strings.ToLower(pattern) {
		case "sdk":
			needsSDK = true
			continue
		case "wdk":
			needsSDK = true
			needsWDK = true
			continue
		}
		for _, id := range knownIDs {
			matched, _ := path.Match(strings.ToLower(pattern), strings.ToLower(id))
			if !matched {
				continue
			}
			needsSDK = true
			if strings.Contains(strings.ToLower(id), ".wdk.") {
				needsWDK = true
			}
		}
	}
	return needsSDK, needsWDK
}

func loadWindowsKitPackage(request nugetPackageRequest, version *string) (vsPackage, error) {
	request.PreferredVersion = *version
	packageInfo, err := loadNativeNuGetPackage(request)
	if err != nil {
		return vsPackage{}, err
	}
	return packageInfo, selectKitVersion(version, packageInfo)
}

func selectKitVersion(selected *string, packageInfo vsPackage) error {
	if *selected == "" {
		*selected = packageInfo.Version
		return nil
	}
	if strings.EqualFold(*selected, packageInfo.Version) {
		return nil
	}
	return fmt.Errorf("Windows Kit NuGet packages have mismatched versions %s and %s", *selected, packageInfo.Version)
}

func latestSDKBuild(packages []vsPackage) string {
	return latestPackageMatch(packages, sdkComponentRE)
}

func latestPackageMatch(packages []vsPackage, expression *regexp.Regexp) string {
	best := ""
	for _, packageInfo := range packages {
		match := expression.FindStringSubmatch(packageInfo.ID)
		if len(match) != 2 {
			continue
		}
		if best == "" || compareVersions(match[1], best) > 0 {
			best = match[1]
		}
	}
	return best
}

func sdkNuGetPackageID(target string) (string, error) {
	names, found := architectures[normalizeArch(target)]
	if !found || names.SDK == "" {
		return "", fmt.Errorf("Windows SDK NuGet does not support target %s", target)
	}
	return "Microsoft.Windows.SDK.cpp." + names.SDK, nil
}

func wdkNuGetPackageID(target string) (string, error) {
	names, found := architectures[normalizeArch(target)]
	if !found || names.WDK == "" {
		return "", fmt.Errorf("WDK NuGet supports x64 and arm64 targets, not %s", target)
	}
	return "Microsoft.Windows.WDK." + names.WDK, nil
}

func loadNativeNuGetPackage(request nugetPackageRequest) (vsPackage, error) {
	client := newHTTPClient(3 * time.Minute)
	lowerID := strings.ToLower(request.ID)
	indexURL := fmt.Sprintf("https://api.nuget.org/v3-flatcontainer/%s/index.json", lowerID)
	data, err := readResource(client, indexURL, 4<<20)
	if err != nil {
		return vsPackage{}, fmt.Errorf("read NuGet versions for %s: %w", request.ID, err)
	}
	var index nugetVersionIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return vsPackage{}, fmt.Errorf("decode NuGet versions for %s: %w", request.ID, err)
	}
	version := selectNativeNuGetVersion(index.Versions, request.SDKBuild, request.Channel, request.PreferredVersion)
	if version == "" {
		return vsPackage{}, fmt.Errorf("NuGet has no %s version matching Windows SDK %s", request.ID, request.SDKBuild)
	}
	lowerVersion := strings.ToLower(version)
	leafURL := fmt.Sprintf("https://api.nuget.org/v3/registration5-semver1/%s/%s.json", lowerID, url.PathEscape(lowerVersion))
	data, err = readResource(client, leafURL, 4<<20)
	if err != nil {
		return vsPackage{}, fmt.Errorf("read NuGet registration for %s %s: %w", request.ID, version, err)
	}
	var leaf nugetRegistrationLeaf
	if err := json.Unmarshal(data, &leaf); err != nil {
		return vsPackage{}, fmt.Errorf("decode NuGet registration for %s %s: %w", request.ID, version, err)
	}
	catalogURL, err := nugetCatalogURL(leaf.CatalogEntry)
	if err != nil {
		return vsPackage{}, fmt.Errorf("decode NuGet catalog URL for %s %s: %w", request.ID, version, err)
	}
	data, err = readResource(client, catalogURL, 4<<20)
	if err != nil {
		return vsPackage{}, fmt.Errorf("read NuGet catalog for %s %s: %w", request.ID, version, err)
	}
	var entry nugetCatalogEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return vsPackage{}, fmt.Errorf("decode NuGet catalog for %s %s: %w", request.ID, version, err)
	}
	if !strings.EqualFold(entry.PackageHashAlgorithm, "SHA512") {
		return vsPackage{}, fmt.Errorf("NuGet %s %s uses unsupported hash %q", request.ID, version, entry.PackageHashAlgorithm)
	}
	hash, err := base64.StdEncoding.DecodeString(entry.PackageHash)
	if err != nil {
		return vsPackage{}, fmt.Errorf("decode NuGet SHA-512 for %s %s: %w", request.ID, version, err)
	}
	productArch := normalizeArch(request.ID[strings.LastIndex(request.ID, ".")+1:])
	if productArch != "x64" && productArch != "arm64" {
		productArch = ""
	}
	return vsPackage{
		ID:            request.ID,
		Version:       version,
		Type:          "Nupkg",
		Chip:          productArch,
		ArchivePrefix: "c/",
		ArchiveTarget: request.ArchiveTarget,
		Payloads: []payload{{
			FileName: strings.ToLower(request.ID) + "." + version + ".nupkg",
			URL:      leaf.PackageContent,
			SHA512:   hex.EncodeToString(hash),
			Size:     entry.PackageSize,
		}},
		LocalizedResources: []localizedResource{{
			Language:    "en-us",
			Title:       entry.Title,
			Description: entry.Description,
			Category:    "Windows Kit",
		}},
	}, nil
}

func nugetCatalogURL(data json.RawMessage) (string, error) {
	var catalogURL string
	if err := json.Unmarshal(data, &catalogURL); err == nil {
		return catalogURL, nil
	}
	var catalogEntry struct {
		URL string `json:"@id"`
	}
	if err := json.Unmarshal(data, &catalogEntry); err != nil {
		return "", err
	}
	if catalogEntry.URL == "" {
		return "", errors.New("registration has no catalog URL")
	}
	return catalogEntry.URL, nil
}

func selectNativeNuGetVersion(versions []string, sdkBuild, channel, preferredVersion string) string {
	prefix := "10.0." + sdkBuild + "."
	allowPrerelease := strings.EqualFold(channel, "insiders") || strings.EqualFold(channel, "preview")
	best := ""
	for _, version := range versions {
		if !strings.HasPrefix(strings.ToLower(version), strings.ToLower(prefix)) {
			continue
		}
		if strings.Contains(version, "-") && !allowPrerelease {
			continue
		}
		if preferredVersion != "" && strings.EqualFold(version, preferredVersion) {
			return version
		}
		if best == "" || compareVersions(version, best) > 0 {
			best = version
		}
	}
	return best
}

func resourceFor(resources []localizedResource, language string) localizedResource {
	for _, resource := range resources {
		if strings.EqualFold(resource.Language, language) {
			return resource
		}
	}
	for _, resource := range resources {
		if strings.EqualFold(resource.Language, "en-us") {
			return resource
		}
	}
	if len(resources) > 0 {
		return resources[0]
	}
	return localizedResource{}
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	var o manifestOptions
	addManifestFlags(fs, &o)
	targetValue := fs.String("target", defaultHost(), "comma-separated target architectures; defaults to the current architecture")
	types := fs.String("type", "Component,Workload,Nupkg", "comma-separated package types or 'all'")
	jsonOutput := fs.Bool("json", false, "emit JSON")
	details := fs.Bool("details", false, "include descriptions and dependency counts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	selectors, err := parsePackageSelection(fs.Args(), []string{"*"})
	if err != nil {
		return err
	}
	resolve := resolveOptions{Manifest: o}
	if err := setResolveArchitectures(&resolve, *targetValue); err != nil {
		return err
	}
	cat, err := loadCatalog(resolve.Manifest)
	if err != nil {
		return err
	}
	if err := addWindowsKitNuGetPackages(cat, resolve, selectors); err != nil {
		return err
	}
	selectors, err = expandPackageAliases(cat, resolve, selectors)
	if err != nil {
		return err
	}
	typeSet := parseTypeSet(*types)
	var out []packageSummary
	seen := map[string]bool{}
	for i := range cat.Manifest.Packages {
		p := &cat.Manifest.Packages[i]
		if !typeSet["all"] && !typeSet[strings.ToLower(p.Type)] {
			continue
		}
		if !matchesPackageSelection(p.ID, selectors) {
			continue
		}
		res := resourceFor(p.LocalizedResources, o.Language)
		key := strings.ToLower(p.Type + "\x00" + p.ID + "\x00" + p.Version)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, packageSummary{
			ID: p.ID, Version: p.Version, Type: p.Type, Title: res.Title,
			Description: res.Description, Category: res.Category,
			Dependencies: len(p.Dependencies), Payloads: len(p.Payloads),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return strings.ToLower(out[i].ID) < strings.ToLower(out[j].ID)
	})
	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	for _, p := range out {
		title := ""
		if p.Title != "" {
			title = " — " + p.Title
		}
		fmt.Printf("%-10s %-18s %s%s\n", p.Type, p.Version, p.ID, title)
		if *details {
			if p.Description != "" {
				fmt.Printf("  %s\n", p.Description)
			}
			fmt.Printf("  category=%s dependencies=%d payloads=%d\n", p.Category, p.Dependencies, p.Payloads)
		}
	}
	fmt.Fprintf(os.Stderr, "%d result(s), catalog %s\n", len(out), cat.Version)
	return nil
}

func expandPackageAliases(cat *catalog, options resolveOptions, selectors []string) ([]string, error) {
	resolver := newResolver(cat, options)
	var expanded []string
	for _, selector := range selectors {
		name, excluded := splitPackageSelector(selector)
		patterns, err := resolver.expandAlias(name)
		if err != nil {
			return nil, err
		}
		for _, pattern := range patterns {
			if excluded {
				pattern = "-" + pattern
			}
			expanded = append(expanded, pattern)
		}
	}
	return expanded, nil
}

func splitPackageSelector(value string) (string, bool) {
	if strings.HasPrefix(value, "-") && value != "-" {
		return strings.TrimPrefix(value, "-"), true
	}
	return value, false
}

type packageSummary struct {
	ID           string `json:"id"`
	Version      string `json:"version"`
	Type         string `json:"type"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"`
	Category     string `json:"category,omitempty"`
	Dependencies int    `json:"dependencies"`
	Payloads     int    `json:"payloads"`
}

func parseTypeSet(v string) map[string]bool {
	result := map[string]bool{}
	for _, item := range strings.Split(v, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item != "" {
			result[item] = true
		}
	}
	return result
}

func matchesPackageSelection(id string, selectors []string) bool {
	included := false
	hasInclusion := false
	lowerID := strings.ToLower(id)
	for _, selector := range selectors {
		pattern, excluded := splitPackageSelector(selector)
		matched, _ := path.Match(strings.ToLower(pattern), lowerID)
		if excluded && matched {
			return false
		}
		if !excluded {
			hasInclusion = true
			included = included || matched
		}
	}
	return included || !hasInclusion
}

type resolveOptions struct {
	Manifest           manifestOptions
	Targets            []string
	IncludeRecommended bool
	IncludeOptional    bool
	Strict             bool
}

type resolution struct {
	Packages []*vsPackage
	Warnings []string
}

type resolver struct {
	catalog  *catalog
	opts     resolveOptions
	byID     map[string][]*vsPackage
	excluded []string
	selected map[string]bool
	visiting map[string]bool
	result   resolution
}

func newResolver(cat *catalog, opts resolveOptions) *resolver {
	r := &resolver{catalog: cat, opts: opts, byID: map[string][]*vsPackage{}, selected: map[string]bool{}, visiting: map[string]bool{}}
	for i := range cat.Manifest.Packages {
		p := &cat.Manifest.Packages[i]
		key := strings.ToLower(p.ID)
		r.byID[key] = append(r.byID[key], p)
	}
	return r
}

func (r *resolver) resolve(roots []string) (*resolution, error) {
	expanded, err := r.expandRoots(roots)
	if err != nil {
		return nil, err
	}
	for _, root := range expanded {
		if err := r.visit(root, dependency{}, true); err != nil {
			return nil, err
		}
	}
	return &r.result, nil
}

func (r *resolver) expandRoots(roots []string) ([]string, error) {
	if len(roots) == 0 {
		roots = []string{"msvc", "sdk"}
	}
	for _, spec := range roots {
		pattern, excluded := splitPackageSelector(spec)
		if !excluded {
			continue
		}
		patterns, err := r.expandAlias(pattern)
		if err != nil {
			return nil, err
		}
		for _, pattern := range patterns {
			if _, err := path.Match(strings.ToLower(pattern), ""); err != nil {
				return nil, fmt.Errorf("invalid exclusion pattern %q: %w", pattern, err)
			}
			r.excluded = append(r.excluded, strings.ToLower(pattern))
		}
	}
	var result []string
	for _, spec := range roots {
		pattern, excluded := splitPackageSelector(spec)
		if excluded {
			continue
		}
		patterns, err := r.expandAlias(pattern)
		if err != nil {
			return nil, err
		}
		for _, pattern := range patterns {
			matches, err := r.matchPackageIDs(pattern)
			if err != nil {
				return nil, err
			}
			for _, id := range matches {
				if _, excluded := r.exclusionFor(id); !excluded {
					result = append(result, id)
				}
			}
		}
	}
	return uniqueStrings(result), nil
}

func (r *resolver) expandAlias(spec string) ([]string, error) {
	switch strings.ToLower(spec) {
	case "msvc":
		toolset := latestMSVCToolset(r.catalog.Manifest.Packages)
		if toolset == "" {
			return nil, errors.New("no MSVC toolset found in catalog")
		}
		host, err := packageArchName(r.opts.Manifest.Host)
		if err != nil {
			return nil, err
		}
		ids := []string{"Microsoft.VC." + toolset + ".CRT.Headers.base"}
		for _, target := range r.opts.Targets {
			targetName, err := packageArchName(target)
			if err != nil {
				return nil, err
			}
			prefix := "Microsoft.VC." + toolset
			ids = append(ids,
				prefix+".Tools.Host"+host+".Target"+targetName+".base",
				prefix+".Tools.Host"+host+".Target"+targetName+".Res.base",
				prefix+".CRT."+targetName+".Desktop.base",
				prefix+".CRT."+targetName+".Store.base",
			)
		}
		return ids, nil
	case "sdk":
		var ids []string
		for _, target := range r.opts.Targets {
			id, err := sdkNuGetPackageID(target)
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, nil
	case "wdk":
		ids := []string{"Microsoft.VisualStudio.WindowsDriverKit.Build"}
		for _, target := range r.opts.Targets {
			wdkPackageID, err := wdkNuGetPackageID(target)
			if err != nil {
				return nil, err
			}
			ids = append(ids, wdkPackageID)
		}
		return ids, nil
	case "wdk-vs":
		return []string{"Component.Microsoft.Windows.DriverKit"}, nil
	case "vctools":
		return []string{"Microsoft.VisualStudio.Workload.VCTools"}, nil
	default:
		return []string{spec}, nil
	}
}

var msvcToolsetPackageRE = regexp.MustCompile(`(?i)^Microsoft\.VC\.(\d+\.\d+\.\d+\.\d+)\.Tools\.Host[^.]+\.Target[^.]+\.base$`)

func latestMSVCToolset(packages []vsPackage) string {
	return latestPackageMatch(packages, msvcToolsetPackageRE)
}

func packageArchName(arch string) (string, error) {
	names, found := architectures[normalizeArch(arch)]
	if !found || names.MSVC == "" {
		return "", fmt.Errorf("unsupported MSVC architecture %s", arch)
	}
	return names.MSVC, nil
}

func (r *resolver) matchPackageIDs(pattern string) ([]string, error) {
	lowerPattern := strings.ToLower(pattern)
	if _, err := path.Match(lowerPattern, ""); err != nil {
		return nil, fmt.Errorf("invalid package pattern %q: %w", pattern, err)
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return []string{pattern}, nil
	}
	var matches []string
	for id := range r.byID {
		matched, _ := path.Match(lowerPattern, id)
		if matched {
			matches = append(matches, r.byID[id][0].ID)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("package pattern %q matched nothing", pattern)
	}
	sort.Slice(matches, func(i, j int) bool { return strings.ToLower(matches[i]) < strings.ToLower(matches[j]) })
	return matches, nil
}

func (r *resolver) exclusionFor(id string) (string, bool) {
	lowerID := strings.ToLower(id)
	for _, pattern := range r.excluded {
		matched, _ := path.Match(pattern, lowerID)
		if matched {
			return pattern, true
		}
	}
	return "", false
}

var sdkComponentRE = regexp.MustCompile(`(?i)^Microsoft\.VisualStudio\.Component\.Windows(?:10|11)SDK\.(\d+)$`)

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, v := range values {
		key := strings.ToLower(v)
		if !seen[key] {
			seen[key] = true
			result = append(result, v)
		}
	}
	return result
}

func (r *resolver) visit(id string, dep dependency, root bool) error {
	if !root && !r.dependencyEnabled(dep) {
		return nil
	}
	if pattern, excluded := r.exclusionFor(id); excluded {
		kind := strings.ToLower(dep.Type)
		if root || kind == "recommended" || kind == "optional" || kind == "excluded" {
			return nil
		}
		return fmt.Errorf("required dependency %s was excluded by pattern -%s", id, pattern)
	}
	candidates := r.candidates(id, dep)
	if len(candidates) == 0 {
		// The catalog often lists alternatives for other machine architectures
		// without an explicit applicability condition. They are not unresolved;
		// they are simply inapplicable to this host/target selection.
		if !root && len(r.byID[strings.ToLower(id)]) > 0 {
			return nil
		}
		message := fmt.Sprintf("dependency %s has no applicable package", id)
		if root || r.opts.Strict {
			return errors.New(message)
		}
		r.result.Warnings = append(r.result.Warnings, message)
		return nil
	}
	for _, p := range candidates {
		key := packageKey(p)
		if r.selected[key] {
			continue
		}
		if r.visiting[key] {
			continue
		}
		r.visiting[key] = true
		depIDs := make([]string, 0, len(p.Dependencies))
		for depID := range p.Dependencies {
			depIDs = append(depIDs, depID)
		}
		sort.Strings(depIDs)
		for _, depID := range depIDs {
			if err := r.visit(depID, p.Dependencies[depID], false); err != nil {
				return fmt.Errorf("%s -> %w", p.ID, err)
			}
		}
		delete(r.visiting, key)
		r.selected[key] = true
		r.result.Packages = append(r.result.Packages, p)
	}
	return nil
}

func (r *resolver) dependencyEnabled(dep dependency) bool {
	kind := strings.ToLower(dep.Type)
	if kind == "recommended" && !r.opts.IncludeRecommended && !r.opts.IncludeOptional {
		return false
	}
	if kind == "optional" && !r.opts.IncludeOptional {
		return false
	}
	if kind == "excluded" {
		return false
	}
	for _, arch := range []string{dep.Chip, dep.ProductArch, dep.MachineArch} {
		if arch != "" && !compatibleArch(arch, r.opts.Manifest.Host, r.opts.Targets) {
			return false
		}
	}
	return true
}

func compatibleArch(value, host string, targets []string) bool {
	value = normalizeArch(value)
	host = normalizeArch(host)
	if value == "" || value == host {
		return true
	}
	if value == "x86" && host == "x64" {
		return true
	}
	for _, target := range targets {
		if value == target {
			return true
		}
	}
	return false
}

func (r *resolver) candidates(id string, dep dependency) []*vsPackage {
	all := r.byID[strings.ToLower(id)]
	if len(all) == 0 {
		return nil
	}
	bestVersion := ""
	for _, p := range all {
		if packageArchScore(p, r.opts.Manifest.Host, r.opts.Targets) < 0 {
			continue
		}
		if p.Language != "" && !strings.EqualFold(p.Language, r.opts.Manifest.Language) {
			continue
		}
		if bestVersion == "" || compareVersions(p.Version, bestVersion) > 0 {
			bestVersion = p.Version
		}
	}
	if bestVersion == "" {
		return nil
	}
	bestByLang := map[string]int{}
	for _, p := range all {
		if p.Version != bestVersion {
			continue
		}
		lang := strings.ToLower(p.Language)
		score := packageArchScore(p, r.opts.Manifest.Host, r.opts.Targets)
		if score > bestByLang[lang] || bestByLang[lang] == 0 {
			bestByLang[lang] = score
		}
	}
	var result []*vsPackage
	for _, p := range all {
		if p.Version != bestVersion {
			continue
		}
		if p.Language != "" && !strings.EqualFold(p.Language, r.opts.Manifest.Language) {
			continue
		}
		score := packageArchScore(p, r.opts.Manifest.Host, r.opts.Targets)
		if score < 0 || score != bestByLang[strings.ToLower(p.Language)] {
			continue
		}
		result = append(result, p)
	}
	return result
}

func packageArchScore(p *vsPackage, host string, targets []string) int {
	score := 1
	host = normalizeArch(host)
	for _, value := range []string{p.MachineArch, p.ProductArch} {
		arch := normalizeArch(value)
		if arch == "" {
			continue
		}
		if arch == host {
			score += 20
		} else if arch == "x86" && host == "x64" {
			score += 5
		} else {
			return -1
		}
	}
	if p.Chip != "" {
		if !compatibleArch(p.Chip, host, targets) {
			return -1
		}
		score += 10
	}
	return score
}

func packageKey(p *vsPackage) string {
	return strings.ToLower(strings.Join([]string{p.ID, p.Version, p.Language, p.Chip, p.MachineArch, p.ProductArch}, "\x00"))
}

var numberRE = regexp.MustCompile(`\d+`)

func compareVersions(a, b string) int {
	aa, bb := numberRE.FindAllString(a, -1), numberRE.FindAllString(b, -1)
	limit := len(aa)
	if len(bb) > limit {
		limit = len(bb)
	}
	for i := 0; i < limit; i++ {
		av, bv := 0, 0
		if i < len(aa) {
			av, _ = strconv.Atoi(aa[i])
		}
		if i < len(bb) {
			bv, _ = strconv.Atoi(bb[i])
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return strings.Compare(strings.ToLower(a), strings.ToLower(b))
}

func parseTargets(value string) ([]string, error) {
	var result []string
	for _, item := range strings.Split(value, ",") {
		arch := normalizeArch(item)
		if arch == "" {
			continue
		}
		if _, found := architectures[arch]; !found {
			return nil, fmt.Errorf("invalid target architecture %q", item)
		}
		result = append(result, arch)
	}
	if len(result) == 0 {
		return nil, errors.New("at least one target is required")
	}
	return uniqueStrings(result), nil
}

func setResolveArchitectures(options *resolveOptions, targetValue string) error {
	host := normalizeArch(options.Manifest.Host)
	if names, found := architectures[host]; !found || !names.Host {
		return fmt.Errorf("invalid host architecture %q", options.Manifest.Host)
	}
	targets, err := parseTargets(targetValue)
	if err != nil {
		return err
	}
	options.Manifest.Host = host
	options.Targets = targets
	return nil
}

func addResolveFlags(fs *flag.FlagSet, o *resolveOptions, targetValue *string) {
	addManifestFlags(fs, &o.Manifest)
	fs.StringVar(targetValue, "target", defaultHost(), "comma-separated target architectures; defaults to the current architecture")
	fs.BoolVar(&o.IncludeRecommended, "include-recommended", false, "include Recommended dependencies")
	fs.BoolVar(&o.IncludeOptional, "include-optional", false, "include Recommended and Optional dependencies")
	fs.BoolVar(&o.Strict, "strict-deps", false, "fail instead of warning on unresolved transitive dependencies")
}

func parsePackageSelection(args, defaults []string) ([]string, error) {
	if len(args) > 1 {
		return nil, errors.New("package selection must be one quoted, space-separated argument")
	}
	if len(args) == 0 {
		return append([]string{}, defaults...), nil
	}
	roots := strings.Fields(args[0])
	if len(roots) == 0 {
		return nil, errors.New("package selection is empty")
	}
	for _, root := range roots {
		pattern, _ := splitPackageSelector(root)
		if _, err := path.Match(strings.ToLower(pattern), ""); err != nil {
			return nil, fmt.Errorf("invalid package pattern %q: %w", root, err)
		}
	}
	return roots, nil
}

func resolvePackages(roots []string, o *resolveOptions, targetValue string) (*catalog, *resolution, error) {
	if err := setResolveArchitectures(o, targetValue); err != nil {
		return nil, nil, err
	}
	cat, err := loadCatalog(o.Manifest)
	if err != nil {
		return nil, nil, err
	}
	if err := addWindowsKitNuGetPackages(cat, *o, roots); err != nil {
		return nil, nil, err
	}
	resolved, err := newResolver(cat, *o).resolve(roots)
	return cat, resolved, err
}

func runResolve(args []string) error {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	var o resolveOptions
	var target string
	addResolveFlags(fs, &o, &target)
	jsonOutput := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	roots, err := parsePackageSelection(fs.Args(), []string{"msvc", "sdk"})
	if err != nil {
		return err
	}
	_, resolved, err := resolvePackages(roots, &o, target)
	if err != nil {
		return err
	}
	return printResolution(resolved, *jsonOutput)
}

func printResolution(resolved *resolution, jsonOutput bool) error {
	var total int64
	payloads := 0
	for _, p := range resolved.Packages {
		for _, item := range p.Payloads {
			total += item.Size
			payloads++
		}
	}
	if jsonOutput {
		result := struct {
			Packages      []*vsPackage `json:"packages"`
			Warnings      []string     `json:"warnings,omitempty"`
			PayloadCount  int          `json:"payloadCount"`
			DownloadBytes int64        `json:"downloadBytes"`
		}{resolved.Packages, resolved.Warnings, payloads, total}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	for _, p := range resolved.Packages {
		fmt.Printf("%-10s %-18s %s", p.Type, p.Version, p.ID)
		if p.Language != "" {
			fmt.Printf(" [%s]", p.Language)
		}
		fmt.Println()
	}
	for _, warning := range resolved.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}
	fmt.Fprintf(os.Stderr, "%d packages, %d payloads, %s download\n", len(resolved.Packages), payloads, formatBytes(total))
	return nil
}

type installOptions struct {
	Resolve              resolveOptions
	CacheDir             string
	LockFile             string
	Jobs                 int
	AllowPayloadMismatch bool
	KeepRaw              bool
	DryRun               bool
	UpdateLock           bool
}

func installPackage(packageInfo *vsPackage, downloaded map[string]string, dest string, keepRaw bool) error {
	cabinets := map[string]string{}
	for _, item := range packageInfo.Payloads {
		if file := downloaded[payloadKey(item)]; file != "" {
			cabinets[strings.ToLower(payloadBaseName(item))] = file
		}
	}
	for _, item := range packageInfo.Payloads {
		file := downloaded[payloadKey(item)]
		if file == "" {
			continue
		}
		if err := installPayload(item, file, dest, packageInfo, cabinets, keepRaw); err != nil {
			return fmt.Errorf("%s: %w", payloadBaseName(item), err)
		}
	}
	return nil
}

func installPayload(item payload, file, dest string, packageInfo *vsPackage, cabinets map[string]string, keepRaw bool) error {
	name := payloadBaseName(item)
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".msi":
		return extractMSI(file, dest, cabinets)
	case ".vsix", ".zip", ".nupkg":
		root, err := packageArchiveRoot(dest, packageInfo)
		if err != nil {
			return err
		}
		return extractZipPayload(file, zipExtractOptions{
			Destination: root,
			Prefix:      packageInfo.ArchivePrefix,
			VSIX:        ext == ".vsix",
		})
	case ".cab":
		return nil
	case ".exe", ".msu":
		if !keepRaw {
			fmt.Fprintf(os.Stderr, "warning: retained in cache but not executed: %s (%s)\n", name, packageInfo.ID)
			return nil
		}
	default:
		if !keepRaw {
			return nil
		}
	}
	rawDest := filepath.Join(dest, "_payloads", safeName(packageInfo.ID), name)
	return copyFile(file, rawDest)
}

func packageArchiveRoot(dest string, packageInfo *vsPackage) (string, error) {
	if packageInfo.ArchiveTarget != "" {
		return safeJoin(dest, packageInfo.ArchiveTarget)
	}
	value := strings.ReplaceAll(packageInfo.ExtensionDir, "\\", "/")
	root := filepath.Join(dest, "_extensions")
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(lower, "[installdir]"):
		root = dest
		value = value[len("[installdir]"):]
	case strings.HasPrefix(lower, "[installroot]"):
		root = dest
		value = value[len("[installroot]"):]
	case value == "":
		return dest, nil
	}
	value = strings.TrimLeft(value, "/")
	if value == "" {
		return root, nil
	}
	return safeJoin(root, value)
}

type installLock struct {
	Format     int         `json:"format"`
	Roots      []string    `json:"roots"`
	Host       string      `json:"host"`
	Targets    []string    `json:"targets"`
	ChannelURL string      `json:"channelUrl,omitempty"`
	LicenseURL string      `json:"licenseUrl,omitempty"`
	Packages   []vsPackage `json:"packages"`
}

func runInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	var o installOptions
	var target string
	addResolveFlags(fs, &o.Resolve, &target)
	fs.StringVar(&o.CacheDir, "cache", "", "download cache directory")
	fs.StringVar(&o.LockFile, "lock-file", "", "reproducible install lock; defaults to DIR.lock")
	fs.IntVar(&o.Jobs, "jobs", 4, "parallel downloads")
	fs.BoolVar(&o.AllowPayloadMismatch, "allow-payload-mismatch", true, "accept catalog size/SHA mismatch after a warning; set false for strict verification")
	fs.BoolVar(&o.KeepRaw, "keep-raw", false, "retain unsupported EXE/MSU payloads under _payloads")
	fs.BoolVar(&o.DryRun, "dry-run", false, "resolve and print without downloading")
	fs.BoolVar(&o.UpdateLock, "update-lock", false, "ignore and replace an existing lock file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	positional := fs.Args()
	if len(positional) == 0 {
		return errors.New("install requires DIR")
	}
	dest, err := filepath.Abs(positional[len(positional)-1])
	if err != nil {
		return err
	}
	if o.LockFile == "" {
		o.LockFile = dest + ".lock"
	} else {
		o.LockFile, err = filepath.Abs(o.LockFile)
		if err != nil {
			return err
		}
	}
	roots, err := parsePackageSelection(positional[:len(positional)-1], []string{"msvc", "sdk"})
	if err != nil {
		return err
	}
	var cat *catalog
	var resolved *resolution
	loadedLock := false
	if !o.UpdateLock {
		cat, resolved, loadedLock, err = readInstallLock(o.LockFile, roots, &o.Resolve, target)
		if err != nil {
			return err
		}
	}
	if !loadedLock {
		cat, resolved, err = resolvePackages(roots, &o.Resolve, target)
		if err != nil {
			return err
		}
	}
	if o.DryRun {
		return printResolution(resolved, false)
	}
	if o.CacheDir == "" {
		cacheBase, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("find user cache: %w", err)
		}
		o.CacheDir = filepath.Join(cacheBase, "msvcup", "cache")
	}
	if o.Jobs < 1 {
		o.Jobs = 1
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(o.CacheDir, 0o755); err != nil {
		return err
	}
	downloads := downloadOptions{
		CacheDir:             o.CacheDir,
		Jobs:                 o.Jobs,
		AllowPayloadMismatch: o.AllowPayloadMismatch,
	}
	files, err := downloadResolution(context.Background(), resolved, downloads)
	if err != nil {
		return err
	}
	for _, p := range resolved.Packages {
		if len(p.Payloads) == 0 {
			continue
		}
		fmt.Printf("install  %s\n", p.ID)
		if err := installPackage(p, files, dest, o.KeepRaw); err != nil {
			return fmt.Errorf("install %s: %w", p.ID, err)
		}
	}
	if err := writeEnvironmentScripts(dest, o.Resolve.Manifest.Host, o.Resolve.Targets); err != nil {
		return err
	}
	if !loadedLock || o.UpdateLock {
		if err := writeInstallLock(o.LockFile, roots, cat, resolved, o.Resolve); err != nil {
			return err
		}
	}
	for _, warning := range resolved.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}
	fmt.Printf("installed %d packages into %s\n", len(resolved.Packages), dest)
	return nil
}

func readInstallLock(path string, roots []string, options *resolveOptions, targetValue string) (*catalog, *resolution, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	var lock installLock
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, nil, false, fmt.Errorf("decode lock %s: %w", path, err)
	}
	if lock.Format != 1 {
		return nil, nil, false, fmt.Errorf("lock %s has unsupported format %d", path, lock.Format)
	}
	if !equalFoldStrings(lock.Roots, roots) {
		return nil, nil, false, fmt.Errorf("lock %s was created for %v, requested %v; use --update-lock to replace it", path, lock.Roots, roots)
	}
	if err := setResolveArchitectures(options, targetValue); err != nil {
		return nil, nil, false, err
	}
	if !equalFoldStrings(lock.Targets, options.Targets) || !strings.EqualFold(lock.Host, options.Manifest.Host) {
		return nil, nil, false, fmt.Errorf("lock %s targets %s/%v, requested %s/%v; use --update-lock to replace it", path, lock.Host, lock.Targets, options.Manifest.Host, options.Targets)
	}
	resolved := &resolution{Packages: make([]*vsPackage, 0, len(lock.Packages))}
	for index := range lock.Packages {
		resolved.Packages = append(resolved.Packages, &lock.Packages[index])
	}
	fmt.Printf("using lock %s\n", path)
	cat := &catalog{ChannelURL: lock.ChannelURL, LicenseURL: lock.LicenseURL}
	return cat, resolved, true, nil
}

func writeInstallLock(path string, roots []string, cat *catalog, resolved *resolution, options resolveOptions) error {
	lock := installLock{
		Format:     1,
		Roots:      append([]string{}, roots...),
		Host:       options.Manifest.Host,
		Targets:    append([]string{}, options.Targets...),
		ChannelURL: cat.ChannelURL,
		LicenseURL: cat.LicenseURL,
		Packages:   make([]vsPackage, 0, len(resolved.Packages)),
	}
	for _, packageInfo := range resolved.Packages {
		lock.Packages = append(lock.Packages, *packageInfo)
	}
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := writeOutput(path, bytes.NewReader(data), 0o644, time.Time{}); err != nil {
		return fmt.Errorf("write lock %s: %w", path, err)
	}
	return nil
}

func equalFoldStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !strings.EqualFold(left[index], right[index]) {
			return false
		}
	}
	return true
}

type downloadTask struct {
	Payload payload
	Path    string
}

type downloadOptions struct {
	CacheDir             string
	Jobs                 int
	AllowPayloadMismatch bool
}

func payloadKey(p payload) string {
	if p.SHA256 != "" {
		return strings.ToLower(p.SHA256)
	}
	if p.SHA512 != "" {
		return strings.ToLower(p.SHA512)
	}
	sum := sha256.Sum256([]byte(p.URL))
	return hex.EncodeToString(sum[:])
}

func downloadResolution(ctx context.Context, resolved *resolution, options downloadOptions) (map[string]string, error) {
	unique := map[string]payload{}
	for _, p := range resolved.Packages {
		for _, item := range p.Payloads {
			if item.URL != "" {
				unique[payloadKey(item)] = item
			}
		}
	}
	tasks := make([]downloadTask, 0, len(unique))
	for key, item := range unique {
		ext := strings.ToLower(filepath.Ext(payloadBaseName(item)))
		if len(ext) > 12 {
			ext = ""
		}
		tasks = append(tasks, downloadTask{Payload: item, Path: filepath.Join(options.CacheDir, key+ext)})
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Payload.FileName < tasks[j].Payload.FileName })
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := make(chan downloadTask)
	type result struct {
		Task downloadTask
		Err  error
	}
	results := make(chan result, len(tasks))
	client := newHTTPClient(0)
	var printMu sync.Mutex
	worker := func() {
		for task := range work {
			printMu.Lock()
			fmt.Printf("fetch    %-48s %10s\n", truncate(payloadBaseName(task.Payload), 48), formatBytes(task.Payload.Size))
			printMu.Unlock()
			err := downloadPayload(ctx, client, task.Payload, task.Path, options.AllowPayloadMismatch)
			results <- result{task, err}
			if err != nil {
				return
			}
		}
	}
	for i := 0; i < options.Jobs; i++ {
		go worker()
	}
	go func() {
		defer close(work)
		for _, task := range tasks {
			select {
			case work <- task:
			case <-ctx.Done():
				return
			}
		}
	}()
	paths := map[string]string{}
	for range tasks {
		res := <-results
		if res.Err != nil {
			cancel()
			return nil, res.Err
		}
		paths[payloadKey(res.Task.Payload)] = res.Task.Path
	}
	return paths, nil
}

func payloadBaseName(p payload) string {
	name := path.Base(strings.ReplaceAll(p.FileName, "\\", "/"))
	if name != "." && name != "" {
		return name
	}
	u, err := url.Parse(p.URL)
	if err == nil {
		name = path.Base(u.Path)
	}
	if name == "" || name == "." {
		return "payload"
	}
	return name
}

func downloadPayload(ctx context.Context, client *http.Client, p payload, dest string, allowMismatch bool) error {
	valid, err := validCachedPayload(dest, p, allowMismatch)
	if err != nil {
		return fmt.Errorf("check cached %s: %w", payloadBaseName(p), err)
	}
	if valid {
		return nil
	}
	if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove invalid cached %s: %w", payloadBaseName(p), err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "msvcup/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", p.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", p.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".msvcup-download-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	hash256 := sha256.New()
	hash512 := sha512.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash256, hash512), resp.Body)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	actualSHA256 := hex.EncodeToString(hash256.Sum(nil))
	actualSHA512 := hex.EncodeToString(hash512.Sum(nil))
	sizeMismatch := p.Size > 0 && n != p.Size
	sha256Mismatch := p.SHA256 != "" && !strings.EqualFold(actualSHA256, p.SHA256)
	sha512Mismatch := p.SHA512 != "" && !strings.EqualFold(actualSHA512, p.SHA512)
	hashMismatch := sha256Mismatch || sha512Mismatch
	if sizeMismatch || hashMismatch {
		if !allowMismatch || p.SHA512 != "" {
			return fmt.Errorf("%s: payload mismatch: got %d bytes SHA-256 %s SHA-512 %s, want %d bytes SHA-256 %s SHA-512 %s", payloadBaseName(p), n, actualSHA256, actualSHA512, p.Size, p.SHA256, p.SHA512)
		}
		fmt.Fprintf(os.Stderr, "warning: accepting payload mismatch for %s: got %d bytes SHA-256 %s\n", payloadBaseName(p), n, actualSHA256)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		if valid, _ := validCachedPayload(dest, p, allowMismatch); !valid {
			return err
		}
	}
	ok = true
	return nil
}

func validCachedPayload(path string, p payload, allowMismatch bool) (bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if allowMismatch && p.SHA512 == "" {
		return info.Size() > 0, nil
	}
	if p.Size > 0 && info.Size() != p.Size {
		return false, nil
	}
	if p.SHA256 == "" && p.SHA512 == "" {
		return true, nil
	}
	if p.SHA512 != "" {
		hash := sha512.New()
		if _, err := io.Copy(hash, file); err != nil {
			return false, err
		}
		return strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), p.SHA512), nil
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false, err
	}
	return strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), p.SHA256), nil
}

func runExtractMSI(args []string) error {
	fs := flag.NewFlagSet("extract-msi", flag.ContinueOnError)
	cabDir := fs.String("cab-dir", "", "directory containing external CAB files")
	var cabs stringList
	fs.Var(&cabs, "cab", "additional CAB path; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("extract-msi requires MSI and DIR")
	}
	msiPath, dest := fs.Arg(0), fs.Arg(1)
	if *cabDir == "" {
		*cabDir = filepath.Dir(msiPath)
	}
	lookup := map[string]string{}
	entries, err := os.ReadDir(*cabDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".cab") {
			lookup[strings.ToLower(entry.Name())] = filepath.Join(*cabDir, entry.Name())
		}
	}
	for _, path := range cabs {
		lookup[strings.ToLower(filepath.Base(path))] = path
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	return extractMSI(msiPath, dest, lookup)
}

type vcvarsConfiguration struct {
	Target    string
	Name      string
	Arguments []string
}

var vcvarsConfigurations = map[string]vcvarsConfiguration{
	"x64_x64":     {Name: "vcvars64.bat", Arguments: []string{"x64", "amd64"}},
	"x64_x86":     {Name: "vcvarsamd64_x86.bat", Arguments: []string{"amd64_x86", "x64_x86"}},
	"x64_arm":     {Name: "vcvarsamd64_arm.bat", Arguments: []string{"amd64_arm", "x64_arm"}},
	"x64_arm64":   {Name: "vcvarsamd64_arm64.bat", Arguments: []string{"amd64_arm64", "x64_arm64"}},
	"x86_x86":     {Name: "vcvars32.bat", Arguments: []string{"x86"}},
	"x86_x64":     {Name: "vcvarsx86_amd64.bat", Arguments: []string{"x86_amd64", "x86_x64"}},
	"x86_arm":     {Name: "vcvarsx86_arm.bat", Arguments: []string{"x86_arm"}},
	"x86_arm64":   {Name: "vcvarsx86_arm64.bat", Arguments: []string{"x86_arm64"}},
	"arm64_arm64": {Name: "vcvarsarm64.bat", Arguments: []string{"arm64"}},
}

func vcvarsForTargets(host string, targets []string) []vcvarsConfiguration {
	var result []vcvarsConfiguration
	host = normalizeArch(host)
	for _, target := range targets {
		target = normalizeArch(target)
		configuration, found := vcvarsConfigurations[host+"_"+target]
		if found {
			configuration.Target = target
			result = append(result, configuration)
		}
	}
	return result
}

func writeEnvironmentScripts(root, host string, targets []string) error {
	msvcVersion := newestVersionDirectory(filepath.Join(root, "VC", "Tools", "MSVC"))
	sdkVersion := newestVersionDirectory(filepath.Join(root, "Windows Kits", "10", "bin"))
	if msvcVersion == "" && sdkVersion == "" {
		return nil
	}
	configurations := vcvarsForTargets(host, targets)
	for _, configuration := range configurations {
		if err := writeBatchEnvironment(root, configuration.Name, host, configuration.Target, msvcVersion, sdkVersion); err != nil {
			return err
		}
	}
	return writeVCVarsAll(root, configurations)
}

var versionDirectoryRE = regexp.MustCompile(`^\d+(?:\.\d+)+$`)

func newestVersionDirectory(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	var versions []string
	for _, entry := range entries {
		if entry.IsDir() && versionDirectoryRE.MatchString(entry.Name()) {
			versions = append(versions, entry.Name())
		}
	}
	sort.Slice(versions, func(i, j int) bool { return compareVersions(versions[i], versions[j]) < 0 })
	if len(versions) == 0 {
		return ""
	}
	return versions[len(versions)-1]
}

func writeBatchEnvironment(root, name, host, target, msvcVersion, sdkVersion string) error {
	var b strings.Builder
	b.WriteString("@echo off\r\n")
	fmt.Fprintf(&b, "set VSCMD_ARG_HOST_ARCH=%s\r\nset VSCMD_ARG_TGT_ARCH=%s\r\n", host, target)
	if msvcVersion != "" {
		fmt.Fprintf(&b, "set VCToolsVersion=%s\r\nset VCToolsInstallDir=%%~dp0VC\\Tools\\MSVC\\%s\\\r\n", msvcVersion, msvcVersion)
		fmt.Fprintf(&b, "set PATH=%%VCToolsInstallDir%%bin\\Host%s\\%s;%%PATH%%\r\n", host, target)
		fmt.Fprintf(&b, "set INCLUDE=%%VCToolsInstallDir%%include;%%INCLUDE%%\r\nset LIB=%%VCToolsInstallDir%%lib\\%s;%%LIB%%\r\n", target)
	}
	if sdkVersion != "" {
		fmt.Fprintf(&b, "set WindowsSdkDir=%%~dp0Windows Kits\\10\\\r\nset WindowsSDKVersion=%s\\\r\n", sdkVersion)
		fmt.Fprintf(&b, "set WindowsSdkVerBinPath=%%WindowsSdkDir%%bin\\%s\\\r\nset WindowsLibPath=%%WindowsSdkDir%%UnionMetadata\\%s;%%WindowsSdkDir%%References\\%s\r\n", sdkVersion, sdkVersion, sdkVersion)
		fmt.Fprintf(&b, "set UniversalCRTSdkDir=%%WindowsSdkDir%%\r\nset UCRTVersion=%s\r\n", sdkVersion)
		fmt.Fprintf(&b, "set PATH=%%WindowsSdkDir%%bin\\%s\\%s;%%PATH%%\r\n", sdkVersion, host)
		fmt.Fprintf(&b, "set INCLUDE=%%WindowsSdkDir%%Include\\%s\\ucrt;%%WindowsSdkDir%%Include\\%s\\shared;%%WindowsSdkDir%%Include\\%s\\um;%%WindowsSdkDir%%Include\\%s\\winrt;%%WindowsSdkDir%%Include\\%s\\km;%%INCLUDE%%\r\n", sdkVersion, sdkVersion, sdkVersion, sdkVersion, sdkVersion)
		fmt.Fprintf(&b, "set LIB=%%WindowsSdkDir%%Lib\\%s\\ucrt\\%s;%%WindowsSdkDir%%Lib\\%s\\um\\%s;%%WindowsSdkDir%%Lib\\%s\\km\\%s;%%LIB%%\r\n", sdkVersion, target, sdkVersion, target, sdkVersion, target)
	}
	return os.WriteFile(filepath.Join(root, name), []byte(b.String()), 0o644)
}

func writeVCVarsAll(root string, configurations []vcvarsConfiguration) error {
	var b strings.Builder
	b.WriteString("@echo off\r\n")
	b.WriteString("set \"_MSVCUP_ARCH=%~1\"\r\n")
	b.WriteString("if not defined _MSVCUP_ARCH set \"_MSVCUP_ARCH=x86\"\r\n")
	for _, configuration := range configurations {
		for _, argument := range configuration.Arguments {
			fmt.Fprintf(&b, "if /i \"%%_MSVCUP_ARCH%%\"==\"%s\" goto %s\r\n", argument, configuration.Target)
		}
	}
	b.WriteString("echo [vcvarsall.bat] Error: unsupported architecture '%_MSVCUP_ARCH%' 1>&2\r\n")
	b.WriteString("set \"_MSVCUP_ARCH=\"\r\nexit /b 1\r\n")
	for _, configuration := range configurations {
		fmt.Fprintf(&b, ":%s\r\nset \"_MSVCUP_ARCH=\"\r\ncall \"%%~dp0%s\"\r\nexit /b %%errorlevel%%\r\n", configuration.Target, configuration.Name)
	}
	return os.WriteFile(filepath.Join(root, "vcvarsall.bat"), []byte(b.String()), 0o644)
}

func formatBytes(value int64) string {
	if value < 0 {
		return "?"
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	v := float64(value)
	unit := 0
	for v >= 1024 && unit < len(units)-1 {
		v /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d %s", value, units[unit])
	}
	return fmt.Sprintf("%.1f %s", v, units[unit])
}

func truncate(value string, length int) string {
	if len(value) <= length {
		return value
	}
	if length <= 1 {
		return value[:length]
	}
	return value[:length-1] + "…"
}
