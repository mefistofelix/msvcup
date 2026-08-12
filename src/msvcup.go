// msvcup downloads and expands portable Microsoft C/C++ toolchains.
package main

import (
	"bytes"
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const version = "0.1.0"

type payload struct {
	FileName string `json:"fileName"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	SHA512   string `json:"sha512,omitempty"`
	Size     int64  `json:"size"`
}

type dependency struct {
	Version     string `json:"version"`
	Type        string `json:"type"`
	Chip        string `json:"chip"`
	ProductArch string `json:"productArch"`
	MachineArch string `json:"machineArch"`
}

func (dependencyInfo *dependency) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &dependencyInfo.Version); err == nil {
		return nil
	}
	type plain dependency
	return json.Unmarshal(data, (*plain)(dependencyInfo))
}

type vsPackage struct {
	ID            string                `json:"id"`
	Version       string                `json:"version"`
	Type          string                `json:"type"`
	Chip          string                `json:"chip"`
	Language      string                `json:"language"`
	MachineArch   string                `json:"machineArch"`
	ProductArch   string                `json:"productArch"`
	ExtensionDir  string                `json:"extensionDir"`
	ArchivePrefix string                `json:"archivePrefix,omitempty"`
	ArchiveTarget string                `json:"archiveTarget,omitempty"`
	Payloads      []payload             `json:"payloads"`
	Dependencies  map[string]dependency `json:"dependencies"`
}

type vsManifest struct {
	EngineVersion string      `json:"engineVersion"`
	Packages      []vsPackage `json:"packages"`
}

type channelItem struct {
	ID       string    `json:"id"`
	Payloads []payload `json:"payloads"`
}

type channelManifest struct {
	Items []channelItem `json:"channelItems"`
}

type catalog struct {
	Manifest   vsManifest
	ChannelURL string
}

type commandOptions struct {
	VS, Channel, Host string
	Targets           []string
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
	case "install":
		return runInstall(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Print(`msvcup - portable Microsoft C/C++ toolchain downloader

Usage:
  msvcup list [options] ["PACKAGE..."]
  msvcup install [options] ["PACKAGE..."] DIR
Aliases: msvc, sdk, wdk.
PACKAGE is one quoted, space-separated selector. Wildcards are case-insensitive;
prefix a selector with - to exclude it. Options precede positional arguments.
`)
}

type architecture struct {
	MSVC string
	SDK  string
	WDK  string
	Host bool
}

var archAliases = map[string]string{
	"amd64": "x64", "x86_64": "x64", "386": "x86", "i386": "x86",
	"i686": "x86", "win32": "x86", "aarch64": "arm64", "neutral": "", "any": "",
}

var architectures = map[string]architecture{
	"x64": {MSVC: "X64", SDK: "x64", WDK: "x64", Host: true},
	"x86": {MSVC: "X86", SDK: "x86", Host: true}, "arm": {MSVC: "ARM", SDK: "ARM"},
	"arm64":   {MSVC: "ARM64", SDK: "ARM64", WDK: "ARM64", Host: true},
	"arm64ec": {MSVC: "ARM64EC"},
}

func normalizeArch(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if alias, found := archAliases[value]; found {
		return alias
	}
	return value
}

func defaultHost() string {
	return normalizeArch(runtime.GOARCH)
}

func setArchitectures(options *commandOptions, targetValue string) error {
	host := normalizeArch(options.Host)
	if names, found := architectures[host]; !found || !names.Host {
		return fmt.Errorf("invalid host architecture %q", options.Host)
	}
	var targets []string
	for _, value := range strings.Split(targetValue, ",") {
		target := normalizeArch(value)
		if target == "" {
			continue
		}
		if _, found := architectures[target]; !found {
			return fmt.Errorf("invalid target architecture %q", value)
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return errors.New("at least one target is required")
	}
	options.Host = host
	options.Targets = unique(targets)
	return nil
}

func addResolveFlags(fs *flag.FlagSet, options *commandOptions, target *string) {
	fs.StringVar(&options.VS, "vs", "18", "Visual Studio family: 18, 17, 16, or latest")
	fs.StringVar(&options.Channel, "channel", "stable", "stable, preview, or an explicit channel URL")
	fs.StringVar(&options.Host, "host", defaultHost(), "host architecture")
	fs.StringVar(target, "target", defaultHost(), "comma-separated target architectures")
}

var channelFamilies = map[string]string{
	"latest": "18", "2026": "18", "18": "18", "2022": "17", "17": "17", "2019": "16", "16": "16",
}

var channelURLs = map[string][2]string{
	"18": {"https://aka.ms/vs/18/stable/channel", "https://aka.ms/vs/18/insiders/channel"},
	"17": {"https://aka.ms/vs/17/release/channel", "https://aka.ms/vs/17/pre/channel"},
	"16": {"https://aka.ms/vs/16/release/channel", "https://aka.ms/vs/16/pre/channel"},
}

func selectedChannelURL(family, channel string) (string, error) {
	channel = strings.ToLower(channel)
	preview := channel == "preview" || channel == "insiders"
	if !preview && channel != "stable" && channel != "latest" && channel != "release" {
		return "", fmt.Errorf("invalid channel %q", channel)
	}
	urls, found := channelURLs[channelFamilies[strings.ToLower(family)]]
	if !found {
		return "", fmt.Errorf("invalid Visual Studio family %q", family)
	}
	if preview {
		return urls[1], nil
	}
	return urls[0], nil
}

var metadataClient = &http.Client{Timeout: 3 * time.Minute}

func webURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func readResource(name string, limit int64) ([]byte, error) {
	if !webURL(name) {
		return os.ReadFile(name)
	}
	response, err := metadataClient.Get(name)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", name, response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("resource %s exceeds %d bytes", name, limit)
	}
	return data, err
}

func readJSON(name string, limit int64, output any) error {
	data, err := readResource(name, limit)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, output)
}

func loadCatalog(options commandOptions) (*catalog, error) {
	channelURL := options.Channel
	explicitURL := webURL(channelURL)
	if !explicitURL {
		var err error
		channelURL, err = selectedChannelURL(options.VS, options.Channel)
		if err != nil {
			return nil, err
		}
	}
	var channel channelManifest
	if err := readJSON(channelURL, 32<<20, &channel); err != nil {
		return nil, fmt.Errorf("read channel: %w", err)
	}
	wanted := "Microsoft.VisualStudio.Manifests.VisualStudio"
	preview := strings.EqualFold(options.Channel, "preview") || strings.EqualFold(options.Channel, "insiders")
	if preview {
		wanted += "Preview"
	}
	var manifestURL string
	for _, item := range channel.Items {
		matches := strings.EqualFold(item.ID, wanted)
		matches = matches || explicitURL && strings.EqualFold(item.ID, "Microsoft.VisualStudio.Manifests.VisualStudioPreview")
		if matches && len(item.Payloads) > 0 {
			manifestURL = item.Payloads[0].URL
			break
		}
	}
	if manifestURL == "" {
		return nil, errors.New("channel has no Visual Studio manifest")
	}
	var manifest vsManifest
	if err := readJSON(manifestURL, 512<<20, &manifest); err != nil {
		return nil, fmt.Errorf("read Visual Studio catalog: %w", err)
	}
	return &catalog{Manifest: manifest, ChannelURL: channelURL}, nil
}

type nugetIndex struct {
	Versions []string `json:"versions"`
}

type nugetRegistration struct {
	CatalogEntry   json.RawMessage `json:"catalogEntry"`
	PackageContent string          `json:"packageContent"`
}

type nugetCatalog struct {
	Hash      string `json:"packageHash"`
	Algorithm string `json:"packageHashAlgorithm"`
	Size      int64  `json:"packageSize"`
}

type kitRequest struct {
	ID, Build, Channel, Version, Target, Dependency string
}

func loadKitPackage(request kitRequest) (vsPackage, error) {
	id := strings.ToLower(request.ID)
	base := "https://api.nuget.org/v3-flatcontainer/" + id + "/"
	var index nugetIndex
	if err := readJSON(base+"index.json", 4<<20, &index); err != nil {
		return vsPackage{}, fmt.Errorf("read NuGet versions for %s: %w", request.ID, err)
	}
	selected := selectKitVersion(index.Versions, request.Build, request.Channel, request.Version)
	if selected == "" {
		return vsPackage{}, fmt.Errorf("NuGet has no %s version matching SDK %s", request.ID, request.Build)
	}
	leafURL := "https://api.nuget.org/v3/registration5-semver1/" + id + "/" + url.PathEscape(strings.ToLower(selected)) + ".json"
	var leaf nugetRegistration
	if err := readJSON(leafURL, 4<<20, &leaf); err != nil {
		return vsPackage{}, fmt.Errorf("read NuGet registration for %s: %w", request.ID, err)
	}
	catalogURL, err := nugetCatalogURL(leaf.CatalogEntry)
	if err != nil {
		return vsPackage{}, fmt.Errorf("read NuGet catalog URL for %s: %w", request.ID, err)
	}
	var entry nugetCatalog
	if err := readJSON(catalogURL, 4<<20, &entry); err != nil {
		return vsPackage{}, fmt.Errorf("read NuGet catalog for %s: %w", request.ID, err)
	}
	if !strings.EqualFold(entry.Algorithm, "SHA512") {
		return vsPackage{}, fmt.Errorf("NuGet %s uses unsupported hash %q", request.ID, entry.Algorithm)
	}
	hash, err := base64.StdEncoding.DecodeString(entry.Hash)
	if err != nil {
		return vsPackage{}, fmt.Errorf("decode NuGet hash for %s: %w", request.ID, err)
	}
	arch := normalizeArch(request.ID[strings.LastIndex(request.ID, ".")+1:])
	if arch != "x64" && arch != "arm64" {
		arch = ""
	}
	return vsPackage{
		ID: request.ID, Version: selected, Type: "Nupkg", Chip: arch,
		ArchivePrefix: "c/", ArchiveTarget: request.Target,
		Payloads: []payload{{
			FileName: id + "." + selected + ".nupkg", URL: leaf.PackageContent,
			SHA512: hex.EncodeToString(hash), Size: entry.Size,
		}},
	}, nil
}

func nugetCatalogURL(data json.RawMessage) (string, error) {
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		return value, nil
	}
	var entry struct {
		URL string `json:"@id"`
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		return "", err
	}
	if entry.URL == "" {
		return "", errors.New("registration has no catalog URL")
	}
	return entry.URL, nil
}

func selectKitVersion(versions []string, build, channel, preferred string) string {
	prefix := "10.0." + build + "."
	prerelease := strings.EqualFold(channel, "preview") || strings.EqualFold(channel, "insiders")
	best := ""
	for _, candidate := range versions {
		if !strings.HasPrefix(strings.ToLower(candidate), strings.ToLower(prefix)) {
			continue
		}
		if strings.Contains(candidate, "-") && !prerelease {
			continue
		}
		if preferred != "" && strings.EqualFold(candidate, preferred) {
			return candidate
		}
		if best == "" || compareVersions(candidate, best) > 0 {
			best = candidate
		}
	}
	return best
}

var sdkComponentRE = regexp.MustCompile(`(?i)^Microsoft\.VisualStudio\.Component\.Windows(?:10|11)SDK\.(\d+)$`)
var msvcToolsetRE = regexp.MustCompile(`(?i)^Microsoft\.VC\.(\d+\.\d+\.\d+\.\d+)\.Tools\.Host[^.]+\.Target[^.]+\.base$`)

func latestPackageMatch(packages []vsPackage, expression *regexp.Regexp) string {
	best := ""
	for _, packageInfo := range packages {
		match := expression.FindStringSubmatch(packageInfo.ID)
		if len(match) == 2 && (best == "" || compareVersions(match[1], best) > 0) {
			best = match[1]
		}
	}
	return best
}

func kitID(kind, target string) (string, error) {
	names := architectures[normalizeArch(target)]
	arch, prefix := names.SDK, "cpp."
	if kind == "WDK" {
		arch, prefix = names.WDK, ""
	}
	if arch == "" {
		return "", fmt.Errorf("Windows %s does not support target %s", kind, target)
	}
	return "Microsoft.Windows." + kind + "." + prefix + arch, nil
}

func requestedKits(selectors, targets []string) (sdk, wdk bool) {
	known := []string{"Microsoft.Windows.SDK.cpp"}
	for _, target := range targets {
		for _, kind := range []string{"SDK", "WDK"} {
			if id, err := kitID(kind, target); err == nil {
				known = append(known, id)
			}
		}
	}
	for _, selector := range selectors {
		pattern, excluded := splitSelector(selector)
		if excluded {
			continue
		}
		switch strings.ToLower(pattern) {
		case "sdk":
			sdk = true
		case "wdk":
			sdk, wdk = true, true
		default:
			for _, id := range known {
				matched, _ := path.Match(strings.ToLower(pattern), strings.ToLower(id))
				sdk = sdk || matched
				wdk = wdk || matched && strings.Contains(strings.ToLower(id), ".wdk.")
			}
		}
	}
	return sdk, wdk
}

func addKitPackages(cat *catalog, options commandOptions, selectors []string) error {
	needsSDK, needsWDK := requestedKits(selectors, options.Targets)
	if !needsSDK {
		return nil
	}
	build := latestPackageMatch(cat.Manifest.Packages, sdkComponentRE)
	if build == "" {
		return errors.New("Windows Kit packages require a numeric SDK component")
	}
	kitRoot := path.Join("Windows Kits", "10")
	requests := []kitRequest{{ID: "Microsoft.Windows.SDK.cpp", Target: kitRoot}}
	for _, target := range options.Targets {
		sdkID, err := kitID("SDK", target)
		if err != nil {
			return err
		}
		requests = append(requests, kitRequest{
			ID: sdkID, Target: path.Join(kitRoot, "Lib", "10.0."+build+".0"), Dependency: "Microsoft.Windows.SDK.cpp",
		})
		if needsWDK {
			wdkID, err := kitID("WDK", target)
			if err != nil {
				return err
			}
			requests = append(requests, kitRequest{ID: wdkID, Target: kitRoot, Dependency: sdkID})
		}
	}
	version := ""
	for _, request := range requests {
		request.Build, request.Channel, request.Version = build, options.Channel, version
		packageInfo, err := loadKitPackage(request)
		if err != nil {
			return err
		}
		if version != "" && !strings.EqualFold(version, packageInfo.Version) {
			return fmt.Errorf("Windows Kit versions differ: %s and %s", version, packageInfo.Version)
		}
		version = packageInfo.Version
		if request.Dependency != "" {
			packageInfo.Dependencies = map[string]dependency{request.Dependency: {Version: version, Type: "Required"}}
		}
		cat.Manifest.Packages = append(cat.Manifest.Packages, packageInfo)
	}
	return nil
}

func splitSelector(value string) (string, bool) {
	if strings.HasPrefix(value, "-") && value != "-" {
		return value[1:], true
	}
	return value, false
}

func parseSelection(args, defaults []string) ([]string, error) {
	if len(args) > 1 {
		return nil, errors.New("package selection must be one quoted argument")
	}
	if len(args) == 0 {
		return slices.Clone(defaults), nil
	}
	selectors := strings.Fields(args[0])
	if len(selectors) == 0 {
		return nil, errors.New("package selection is empty")
	}
	return selectors, nil
}

type resolver struct {
	options  commandOptions
	byID     map[string][]*vsPackage
	toolset  string
	excluded []string
	selected map[string]bool
	visiting map[string]bool
	packages []*vsPackage
}

func newResolver(cat *catalog, options commandOptions) *resolver {
	result := &resolver{
		options: options, byID: map[string][]*vsPackage{},
		toolset:  latestPackageMatch(cat.Manifest.Packages, msvcToolsetRE),
		selected: map[string]bool{}, visiting: map[string]bool{},
	}
	for index := range cat.Manifest.Packages {
		packageInfo := &cat.Manifest.Packages[index]
		key := strings.ToLower(packageInfo.ID)
		result.byID[key] = append(result.byID[key], packageInfo)
	}
	return result
}

func (resolverInfo *resolver) alias(value string) ([]string, error) {
	switch strings.ToLower(value) {
	case "msvc":
		if resolverInfo.toolset == "" {
			return nil, errors.New("no MSVC toolset found")
		}
		prefix := "Microsoft.VC." + resolverInfo.toolset
		host := architectures[resolverInfo.options.Host].MSVC
		ids := []string{prefix + ".CRT.Headers.base"}
		for _, target := range resolverInfo.options.Targets {
			name := architectures[target].MSVC
			ids = append(ids, prefix+".Tools.Host"+host+".Target"+name+".base",
				prefix+".Tools.Host"+host+".Target"+name+".Res.base",
				prefix+".CRT."+name+".Desktop.base", prefix+".CRT."+name+".Store.base")
		}
		return ids, nil
	case "sdk", "wdk":
		kind := strings.ToUpper(value)
		ids := []string{}
		if kind == "WDK" {
			ids = append(ids, "Microsoft.VisualStudio.WindowsDriverKit.Build")
		}
		for _, target := range resolverInfo.options.Targets {
			if kind == "WDK" {
				sdkID, err := kitID("SDK", target)
				if err != nil {
					return nil, err
				}
				ids = append(ids, sdkID)
			}
			id, err := kitID(kind, target)
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, nil
	default:
		return []string{value}, nil
	}
}

func (resolverInfo *resolver) match(pattern string) ([]string, error) {
	if !strings.ContainsAny(pattern, "*?[") {
		return []string{pattern}, nil
	}
	var matches []string
	for id, variants := range resolverInfo.byID {
		matched, _ := path.Match(strings.ToLower(pattern), id)
		if matched {
			matches = append(matches, variants[0].ID)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("package pattern %q matched nothing", pattern)
	}
	sort.Slice(matches, func(left, right int) bool { return strings.ToLower(matches[left]) < strings.ToLower(matches[right]) })
	return matches, nil
}

func (resolverInfo *resolver) isExcluded(id string) bool {
	for _, pattern := range resolverInfo.excluded {
		matched, _ := path.Match(pattern, strings.ToLower(id))
		if matched {
			return true
		}
	}
	return false
}

func (resolverInfo *resolver) expandSelectors(selectors []string) ([]string, error) {
	var expanded []string
	for _, selector := range selectors {
		value, excluded := splitSelector(selector)
		aliases, err := resolverInfo.alias(value)
		if err != nil {
			return nil, err
		}
		for _, alias := range aliases {
			if excluded {
				alias = "-" + alias
			}
			expanded = append(expanded, alias)
		}
	}
	return expanded, nil
}

func (resolverInfo *resolver) resolve(selectors []string) ([]*vsPackage, error) {
	expanded, err := resolverInfo.expandSelectors(selectors)
	if err != nil {
		return nil, err
	}
	for _, selector := range expanded {
		pattern, excluded := splitSelector(selector)
		if excluded {
			resolverInfo.excluded = append(resolverInfo.excluded, strings.ToLower(pattern))
		}
	}
	for _, selector := range expanded {
		pattern, excluded := splitSelector(selector)
		if excluded {
			continue
		}
		matches, err := resolverInfo.match(pattern)
		if err != nil {
			return nil, err
		}
		for _, id := range matches {
			if resolverInfo.isExcluded(id) {
				continue
			}
			if err := resolverInfo.visit(id); err != nil {
				return nil, err
			}
		}
	}
	return resolverInfo.packages, nil
}

func (resolverInfo *resolver) visit(id string) error {
	key := strings.ToLower(id)
	if resolverInfo.selected[key] || resolverInfo.visiting[key] {
		return nil
	}
	if resolverInfo.isExcluded(id) {
		return fmt.Errorf("required dependency %s was excluded", id)
	}
	packageInfo := resolverInfo.bestPackage(key)
	if packageInfo == nil {
		if len(resolverInfo.byID[key]) > 0 {
			return nil
		}
		return fmt.Errorf("package %s was not found", id)
	}
	resolverInfo.visiting[key] = true
	dependencies := make([]string, 0, len(packageInfo.Dependencies))
	for dependencyID := range packageInfo.Dependencies {
		dependencies = append(dependencies, dependencyID)
	}
	sort.Strings(dependencies)
	for _, dependencyID := range dependencies {
		dependencyInfo := packageInfo.Dependencies[dependencyID]
		if !resolverInfo.wants(dependencyInfo) {
			continue
		}
		if err := resolverInfo.visit(dependencyID); err != nil {
			return fmt.Errorf("%s -> %w", packageInfo.ID, err)
		}
	}
	delete(resolverInfo.visiting, key)
	resolverInfo.selected[key] = true
	resolverInfo.packages = append(resolverInfo.packages, packageInfo)
	return nil
}

func (resolverInfo *resolver) wants(dependencyInfo dependency) bool {
	kind := strings.ToLower(dependencyInfo.Type)
	if kind == "recommended" || kind == "optional" || kind == "excluded" {
		return false
	}
	for _, value := range []string{dependencyInfo.Chip, dependencyInfo.ProductArch, dependencyInfo.MachineArch} {
		if value != "" && !compatibleArch(value, resolverInfo.options.Host, resolverInfo.options.Targets) {
			return false
		}
	}
	return true
}

func compatibleArch(value, host string, targets []string) bool {
	value, host = normalizeArch(value), normalizeArch(host)
	if value == "" || value == host || value == "x86" && host == "x64" {
		return true
	}
	return slices.Contains(targets, value)
}

func (resolverInfo *resolver) bestPackage(id string) *vsPackage {
	var best *vsPackage
	bestScore := -1
	for _, packageInfo := range resolverInfo.byID[id] {
		score := packageScore(packageInfo, resolverInfo.options)
		if score < 0 {
			continue
		}
		newer := best == nil || compareVersions(packageInfo.Version, best.Version) > 0
		sameBetter := best != nil && packageInfo.Version == best.Version && score > bestScore
		if newer || sameBetter {
			best, bestScore = packageInfo, score
		}
	}
	return best
}

func packageScore(packageInfo *vsPackage, options commandOptions) int {
	score := 1
	if packageInfo.Language != "" {
		if !strings.EqualFold(packageInfo.Language, "en-us") {
			return -1
		}
		score++
	}
	for _, value := range []string{packageInfo.MachineArch, packageInfo.ProductArch} {
		arch := normalizeArch(value)
		switch {
		case arch == "":
		case arch == options.Host:
			score += 20
		case arch == "x86" && options.Host == "x64":
			score += 5
		default:
			return -1
		}
	}
	if packageInfo.Chip != "" && !compatibleArch(packageInfo.Chip, options.Host, options.Targets) {
		return -1
	}
	return score
}

func unique(values []string) []string {
	seen := map[string]bool{}
	result := values[:0]
	for _, value := range values {
		key := strings.ToLower(value)
		if !seen[key] {
			seen[key] = true
			result = append(result, value)
		}
	}
	return result
}

func resolvePackages(selectors []string, options *commandOptions, target string) (*catalog, []*vsPackage, error) {
	if err := setArchitectures(options, target); err != nil {
		return nil, nil, err
	}
	cat, err := loadCatalog(*options)
	if err != nil {
		return nil, nil, err
	}
	if err := addKitPackages(cat, *options, selectors); err != nil {
		return nil, nil, err
	}
	resolved, err := newResolver(cat, *options).resolve(selectors)
	return cat, resolved, err
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	var options commandOptions
	var target string
	addResolveFlags(fs, &options, &target)
	if err := fs.Parse(args); err != nil {
		return err
	}
	selectors, err := parseSelection(fs.Args(), []string{"*"})
	if err != nil {
		return err
	}
	if err := setArchitectures(&options, target); err != nil {
		return err
	}
	cat, err := loadCatalog(options)
	if err != nil {
		return err
	}
	if err := addKitPackages(cat, options, selectors); err != nil {
		return err
	}
	resolverInfo := newResolver(cat, options)
	selectors, err = resolverInfo.expandSelectors(selectors)
	if err != nil {
		return err
	}
	var packages []*vsPackage
	for id := range resolverInfo.byID {
		packageInfo := resolverInfo.bestPackage(id)
		if packageInfo != nil && matchesSelection(packageInfo.ID, selectors) {
			packages = append(packages, packageInfo)
		}
	}
	sort.Slice(packages, func(left, right int) bool {
		return strings.ToLower(packages[left].ID) < strings.ToLower(packages[right].ID)
	})
	for _, packageInfo := range packages {
		fmt.Printf("%-10s %-18s %s\n", packageInfo.Type, packageInfo.Version, packageInfo.ID)
	}
	fmt.Fprintf(os.Stderr, "%d result(s)\n", len(packages))
	return nil
}

func matchesSelection(id string, selectors []string) bool {
	included, hasInclusion := false, false
	for _, selector := range selectors {
		pattern, excluded := splitSelector(selector)
		matched, _ := path.Match(strings.ToLower(pattern), strings.ToLower(id))
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

type installOptions struct {
	Resolve              commandOptions
	AllowPayloadMismatch bool
	UpdateLock           bool
}

type installLock struct {
	Format     int         `json:"format"`
	Complete   bool        `json:"complete"`
	Roots      []string    `json:"roots"`
	Host       string      `json:"host"`
	Targets    []string    `json:"targets"`
	ChannelURL string      `json:"channelUrl,omitempty"`
	Packages   []vsPackage `json:"packages"`
}

func runInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	var options installOptions
	var target string
	addResolveFlags(fs, &options.Resolve, &target)
	fs.BoolVar(&options.AllowPayloadMismatch, "allow-payload-mismatch", true, "accept Visual Studio catalog mismatches")
	fs.BoolVar(&options.UpdateLock, "update-lock", false, "replace an existing lock")
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
	selectors, err := parseSelection(positional[:len(positional)-1], []string{"msvc", "sdk"})
	if err != nil {
		return err
	}
	if err := setArchitectures(&options.Resolve, target); err != nil {
		return err
	}
	lockFile := dest + ".lock"
	var cat *catalog
	var resolved []*vsPackage
	locked := false
	if !options.UpdateLock {
		cat, resolved, locked, err = readLock(lockFile, selectors, options.Resolve)
		if err != nil {
			return err
		}
	}
	if locked {
		info, statErr := os.Stat(dest)
		switch {
		case statErr == nil && info.IsDir():
			fmt.Printf("already installed %d packages in %s\n", len(resolved), dest)
			return nil
		case statErr == nil:
			return fmt.Errorf("destination %s is not a directory", dest)
		case !errors.Is(statErr, os.ErrNotExist):
			return statErr
		}
		if err := os.Remove(lockFile); err != nil {
			return fmt.Errorf("remove stale lock %s: %w", lockFile, err)
		}
	}
	if options.UpdateLock || !locked {
		cat, resolved, err = resolvePackages(selectors, &options.Resolve, target)
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	downloadDir, err := os.MkdirTemp("", "msvcup-downloads-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(downloadDir)
	files, err := downloadResolution(resolved, downloadDir, options.AllowPayloadMismatch)
	if err != nil {
		return err
	}
	if options.UpdateLock {
		if err := os.Remove(lockFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("invalidate lock %s: %w", lockFile, err)
		}
	}
	for _, packageInfo := range resolved {
		if len(packageInfo.Payloads) == 0 {
			continue
		}
		fmt.Printf("install  %s\n", packageInfo.ID)
		if err := installPackage(packageInfo, files, dest); err != nil {
			return fmt.Errorf("install %s: %w", packageInfo.ID, err)
		}
	}
	if err := writeEnvironmentScripts(dest, options.Resolve.Host, options.Resolve.Targets); err != nil {
		return err
	}
	if err := writeLock(lockFile, selectors, cat, resolved, options.Resolve); err != nil {
		return err
	}
	fmt.Printf("installed %d packages into %s\n", len(resolved), dest)
	return nil
}

func readLock(name string, roots []string, options commandOptions) (*catalog, []*vsPackage, bool, error) {
	data, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	var lock installLock
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, nil, false, fmt.Errorf("decode lock %s: %w", name, err)
	}
	equal := func(left, right string) bool { return strings.EqualFold(left, right) }
	if lock.Format != 2 || !lock.Complete || !slices.EqualFunc(lock.Roots, roots, equal) ||
		!slices.EqualFunc(lock.Targets, options.Targets, equal) || !equal(lock.Host, options.Host) {
		return nil, nil, false, fmt.Errorf("lock %s does not match this request; use --update-lock", name)
	}
	resolved := make([]*vsPackage, len(lock.Packages))
	for index := range lock.Packages {
		resolved[index] = &lock.Packages[index]
	}
	fmt.Printf("using lock %s\n", name)
	return &catalog{ChannelURL: lock.ChannelURL}, resolved, true, nil
}

func writeLock(name string, roots []string, cat *catalog, resolved []*vsPackage, options commandOptions) error {
	lock := installLock{Format: 2, Complete: true, Roots: slices.Clone(roots), Host: options.Host,
		Targets: slices.Clone(options.Targets), ChannelURL: cat.ChannelURL}
	for _, packageInfo := range resolved {
		lock.Packages = append(lock.Packages, *packageInfo)
	}
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(name, append(data, '\n'))
}

func writeAtomic(name string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(name), ".msvcup-lock-*")
	if err != nil {
		return err
	}
	temporaryName := file.Name()
	defer os.Remove(temporaryName)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Chmod(0o644); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, name); err != nil {
		return fmt.Errorf("replace lock %s: %w", name, err)
	}
	return nil
}

func payloadName(item payload) string {
	name := path.Base(strings.ReplaceAll(item.FileName, "\\", "/"))
	if name == "" || name == "." {
		if parsed, err := url.Parse(item.URL); err == nil {
			name = path.Base(parsed.Path)
		}
	}
	if name == "" || name == "." {
		return "payload"
	}
	return name
}

func payloadKey(item payload) string {
	if item.SHA512 != "" {
		return strings.ToLower(item.SHA512)
	}
	if item.SHA256 != "" {
		return strings.ToLower(item.SHA256)
	}
	hash := sha256.Sum256([]byte(item.URL))
	return hex.EncodeToString(hash[:])
}

func downloadResolution(resolved []*vsPackage, directory string, allowMismatch bool) (map[string]string, error) {
	items := map[string]payload{}
	for _, packageInfo := range resolved {
		for _, item := range packageInfo.Payloads {
			if item.URL != "" {
				items[payloadKey(item)] = item
			}
		}
	}
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	files := map[string]string{}
	client := &http.Client{}
	for _, key := range keys {
		item := items[key]
		extension := filepath.Ext(payloadName(item))
		file := filepath.Join(directory, key+extension)
		fmt.Printf("fetch    %s (%s)\n", payloadName(item), formatBytes(item.Size))
		if err := downloadPayload(client, item, file, allowMismatch); err != nil {
			return nil, err
		}
		files[key] = file
	}
	return files, nil
}

func downloadPayload(client *http.Client, item payload, file string, allowMismatch bool) error {
	request, err := http.NewRequest(http.MethodGet, item.URL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "msvcup/"+version)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("GET %s: %w", item.URL, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", item.URL, response.Status)
	}
	output, err := os.Create(file)
	if err != nil {
		return err
	}
	hash256, hash512 := sha256.New(), sha512.New()
	size, copyErr := io.Copy(io.MultiWriter(output, hash256, hash512), response.Body)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	actual256, actual512 := hex.EncodeToString(hash256.Sum(nil)), hex.EncodeToString(hash512.Sum(nil))
	mismatch := item.Size > 0 && item.Size != size
	mismatch = mismatch || item.SHA256 != "" && !strings.EqualFold(item.SHA256, actual256)
	mismatch = mismatch || item.SHA512 != "" && !strings.EqualFold(item.SHA512, actual512)
	if !mismatch {
		return nil
	}
	if !allowMismatch || item.SHA512 != "" {
		return fmt.Errorf("%s: payload mismatch: got %d bytes SHA-256 %s SHA-512 %s", payloadName(item), size, actual256, actual512)
	}
	fmt.Fprintf(os.Stderr, "warning: accepting catalog mismatch for %s: SHA-256 %s\n", payloadName(item), actual256)
	return nil
}

func installPackage(packageInfo *vsPackage, files map[string]string, dest string) error {
	cabinets := map[string]string{}
	for _, item := range packageInfo.Payloads {
		if file := files[payloadKey(item)]; file != "" {
			cabinets[strings.ToLower(payloadName(item))] = file
		}
	}
	for _, item := range packageInfo.Payloads {
		file := files[payloadKey(item)]
		if file == "" {
			continue
		}
		extension := strings.ToLower(filepath.Ext(payloadName(item)))
		switch extension {
		case ".msi":
			if err := extractMSI(file, dest, cabinets); err != nil {
				return err
			}
		case ".vsix", ".zip", ".nupkg":
			root, err := packageArchiveRoot(dest, packageInfo)
			if err != nil {
				return err
			}
			if err := extractZipPayload(file, zipExtractOptions{Destination: root, Prefix: packageInfo.ArchivePrefix, VSIX: extension == ".vsix"}); err != nil {
				return err
			}
		case ".exe", ".msu":
			fmt.Fprintf(os.Stderr, "warning: payload not executed: %s (%s)\n", payloadName(item), packageInfo.ID)
		}
	}
	return nil
}

func packageArchiveRoot(dest string, packageInfo *vsPackage) (string, error) {
	if packageInfo.ArchiveTarget != "" {
		return safeJoin(dest, packageInfo.ArchiveTarget)
	}
	value := strings.ReplaceAll(packageInfo.ExtensionDir, "\\", "/")
	if value == "" {
		return dest, nil
	}
	root := filepath.Join(dest, "_extensions")
	for _, token := range []string{"[installdir]", "[installroot]"} {
		if strings.HasPrefix(strings.ToLower(value), token) {
			root, value = dest, value[len(token):]
			break
		}
	}
	value = strings.TrimLeft(value, "/")
	if value == "" {
		return root, nil
	}
	return safeJoin(root, value)
}

type vcvarsConfiguration struct {
	Host, Target, Name string
	Arguments          []string
}

var vcvarsConfigurations = []vcvarsConfiguration{
	{Host: "x64", Target: "x64", Name: "vcvars64.bat", Arguments: []string{"x64", "amd64"}},
	{Host: "x64", Target: "x86", Name: "vcvarsamd64_x86.bat", Arguments: []string{"amd64_x86", "x64_x86"}},
	{Host: "x64", Target: "arm", Name: "vcvarsamd64_arm.bat", Arguments: []string{"amd64_arm", "x64_arm"}},
	{Host: "x64", Target: "arm64", Name: "vcvarsamd64_arm64.bat", Arguments: []string{"amd64_arm64", "x64_arm64"}},
	{Host: "x86", Target: "x86", Name: "vcvars32.bat", Arguments: []string{"x86"}},
	{Host: "x86", Target: "x64", Name: "vcvarsx86_amd64.bat", Arguments: []string{"x86_amd64", "x86_x64"}},
	{Host: "x86", Target: "arm", Name: "vcvarsx86_arm.bat", Arguments: []string{"x86_arm"}},
	{Host: "x86", Target: "arm64", Name: "vcvarsx86_arm64.bat", Arguments: []string{"x86_arm64"}},
	{Host: "arm64", Target: "arm64", Name: "vcvarsarm64.bat", Arguments: []string{"arm64"}},
}

type vcvarsData struct {
	vcvarsConfiguration
	MSVC, SDK string
}

var vcvarsTemplates = template.Must(template.New("vcvars").Parse(`{{define "vcvars"}}@echo off
set VSCMD_ARG_HOST_ARCH={{.Host}}
set VSCMD_ARG_TGT_ARCH={{.Target}}{{if .MSVC}}
set VCToolsVersion={{.MSVC}}
set VCToolsInstallDir=%~dp0VC\Tools\MSVC\{{.MSVC}}\
set PATH=%VCToolsInstallDir%bin\Host{{.Host}}\{{.Target}};%PATH%
set INCLUDE=%VCToolsInstallDir%include;%INCLUDE%
set LIB=%VCToolsInstallDir%lib\{{.Target}};%LIB%{{end}}{{if .SDK}}
set WindowsSdkDir=%~dp0Windows Kits\10\
set WindowsSDKVersion={{.SDK}}\
set WindowsSdkVerBinPath=%WindowsSdkDir%bin\{{.SDK}}\
set WindowsLibPath=%WindowsSdkDir%UnionMetadata\{{.SDK}};%WindowsSdkDir%References\{{.SDK}}
set UniversalCRTSdkDir=%WindowsSdkDir%
set UCRTVersion={{.SDK}}
set PATH=%WindowsSdkDir%bin\{{.SDK}}\{{.Host}};%PATH%
set INCLUDE=%WindowsSdkDir%Include\{{.SDK}}\ucrt;%WindowsSdkDir%Include\{{.SDK}}\shared;%WindowsSdkDir%Include\{{.SDK}}\um;%WindowsSdkDir%Include\{{.SDK}}\winrt;%WindowsSdkDir%Include\{{.SDK}}\km;%INCLUDE%
set LIB=%WindowsSdkDir%Lib\{{.SDK}}\ucrt\{{.Target}};%WindowsSdkDir%Lib\{{.SDK}}\um\{{.Target}};%WindowsSdkDir%Lib\{{.SDK}}\km\{{.Target}};%LIB%{{end}}
{{end}}{{define "vcvarsall"}}@echo off
set "_MSVCUP_ARCH=%~1"
if not defined _MSVCUP_ARCH set "_MSVCUP_ARCH=x86"{{range $configuration := .}}{{range .Arguments}}
if /i "%_MSVCUP_ARCH%"=="{{.}}" goto {{$configuration.Target}}{{end}}{{end}}
echo [vcvarsall.bat] Error: unsupported architecture '%_MSVCUP_ARCH%' 1>&2
set "_MSVCUP_ARCH="
exit /b 1{{range .}}
:{{.Target}}
set "_MSVCUP_ARCH="
call "%~dp0{{.Name}}"
exit /b %errorlevel%{{end}}
{{end}}`))

func newestVersionDirectory(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	best := ""
	for _, entry := range entries {
		if entry.IsDir() && versionDirectoryRE.MatchString(entry.Name()) && compareVersions(entry.Name(), best) > 0 {
			best = entry.Name()
		}
	}
	return best
}

func renderBatch(name, templateName string, value any) error {
	var output bytes.Buffer
	if err := vcvarsTemplates.ExecuteTemplate(&output, templateName, value); err != nil {
		return err
	}
	data := strings.ReplaceAll(output.String(), "\n", "\r\n")
	return os.WriteFile(name, []byte(data), 0o644)
}

func writeEnvironmentScripts(root, host string, targets []string) error {
	msvc := newestVersionDirectory(filepath.Join(root, "VC", "Tools", "MSVC"))
	sdk := newestVersionDirectory(filepath.Join(root, "Windows Kits", "10", "bin"))
	var selected []vcvarsConfiguration
	for _, configuration := range vcvarsConfigurations {
		if configuration.Host == host && slices.Contains(targets, configuration.Target) {
			selected = append(selected, configuration)
			if err := renderBatch(filepath.Join(root, configuration.Name), "vcvars", vcvarsData{configuration, msvc, sdk}); err != nil {
				return err
			}
		}
	}
	if len(selected) == 0 || msvc == "" && sdk == "" {
		return nil
	}
	return renderBatch(filepath.Join(root, "vcvarsall.bat"), "vcvarsall", selected)
}

var numberRE = regexp.MustCompile(`\d+`)
var versionDirectoryRE = regexp.MustCompile(`^\d+(?:\.\d+)+$`)

func compareVersions(left, right string) int {
	leftNumbers, rightNumbers := numberRE.FindAllString(left, -1), numberRE.FindAllString(right, -1)
	for index := range max(len(leftNumbers), len(rightNumbers)) {
		leftValue, rightValue := 0, 0
		if index < len(leftNumbers) {
			leftValue, _ = strconv.Atoi(leftNumbers[index])
		}
		if index < len(rightNumbers) {
			rightValue, _ = strconv.Atoi(rightNumbers[index])
		}
		if leftValue != rightValue {
			return leftValue - rightValue
		}
	}
	return strings.Compare(strings.ToLower(left), strings.ToLower(right))
}

func formatBytes(value int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	amount, unit := float64(value), 0
	for amount >= 1024 && unit < len(units)-1 {
		amount, unit = amount/1024, unit+1
	}
	if unit == 0 {
		return fmt.Sprintf("%d %s", value, units[unit])
	}
	return fmt.Sprintf("%.1f %s", amount, units[unit])
}
