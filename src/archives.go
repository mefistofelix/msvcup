// Archive and installer-container handling for msvcup.
package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	winmsi "github.com/ArchangelX360/winarchive-tools/msi"
	"github.com/abemedia/go-cabinet"
	"github.com/richardlehane/mscfb"
)

type zipExtractOptions struct {
	Destination string
	Prefix      string
	VSIX        bool
}

func extractZipPayload(archivePath string, options zipExtractOptions) error {
	archiveReader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer archiveReader.Close()
	hasContents := false
	for _, archiveFile := range archiveReader.File {
		name := strings.ReplaceAll(archiveFile.Name, "\\", "/")
		if strings.HasPrefix(strings.ToLower(name), "contents/") {
			hasContents = true
			break
		}
	}
	prefix := strings.TrimLeft(strings.ReplaceAll(options.Prefix, "\\", "/"), "/")
	for _, archiveFile := range archiveReader.File {
		if archiveFile.FileInfo().IsDir() {
			continue
		}
		name, include := zipEntryPath(archiveFile.Name, prefix, hasContents, options.VSIX)
		if !include {
			continue
		}
		if decoded, err := url.PathUnescape(name); err == nil {
			name = decoded
		}
		out, err := safeJoin(options.Destination, name)
		if err != nil {
			return fmt.Errorf("unsafe ZIP entry %q: %w", archiveFile.Name, err)
		}
		content, err := archiveFile.Open()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.EqualFold(filepath.Ext(out), ".exe") {
			mode = 0o755
		}
		if err := writeArchiveOutput(out, content, mode, archiveFile.Modified); err != nil {
			return err
		}
	}
	return nil
}

func zipEntryPath(name, prefix string, hasContents, vsix bool) (string, bool) {
	name = strings.ReplaceAll(name, "\\", "/")
	if prefix != "" {
		return trimArchivePrefix(name, prefix)
	}
	if hasContents {
		return trimArchivePrefix(name, "contents/")
	}
	return name, !vsix || !isVSIXMetadata(name)
}

func trimArchivePrefix(name, prefix string) (string, bool) {
	if len(name) < len(prefix) || !strings.EqualFold(name[:len(prefix)], prefix) {
		return "", false
	}
	return name[len(prefix):], true
}

func isVSIXMetadata(name string) bool {
	lower := strings.ToLower(strings.ReplaceAll(name, "\\", "/"))
	return strings.HasPrefix(lower, "_rels/") ||
		strings.HasPrefix(lower, "package/services/") ||
		lower == "[content_types].xml" || lower == "extension.vsixmanifest" ||
		lower == "manifest.json" || lower == "catalog.json"
}

func extractMSI(msiPath, dest string, cabLookup map[string]string) error {
	parsed, err := parseMSISafely(msiPath)
	if err != nil {
		return err
	}
	if len(parsed.CABFiles) == 0 {
		if len(parsed.FileMap) > 0 {
			fmt.Fprintf(os.Stderr, "warning: MSI lists %d loose files and has no cabinets; no embedded content to extract\n", len(parsed.FileMap))
		}
		return nil
	}
	fileMap := make(map[string]string, len(parsed.FileMap))
	for key, value := range parsed.FileMap {
		fileMap[strings.ToLower(key)] = value
	}
	var cabPaths []string
	tempDir := ""
	defer func() {
		if tempDir != "" {
			_ = os.RemoveAll(tempDir)
		}
	}()
	for _, name := range parsed.CABFiles {
		embeddedName, embedded := strings.CutPrefix(name, "#")
		if !embedded {
			cabPath := cabLookup[strings.ToLower(path.Base(strings.ReplaceAll(name, "\\", "/")))]
			if cabPath == "" {
				return fmt.Errorf("MSI requires missing CAB %q", name)
			}
			cabPaths = append(cabPaths, cabPath)
			continue
		}
		if tempDir == "" {
			tempDir, err = os.MkdirTemp("", "msvcup-embedded-cab-*")
			if err != nil {
				return err
			}
		}
		cabPath := filepath.Join(tempDir, safeName(embeddedName)+".cab")
		if err := extractEmbeddedMSIStream(msiPath, embeddedName, cabPath); err != nil {
			return err
		}
		cabPaths = append(cabPaths, cabPath)
	}
	extracted := map[string]bool{}
	var unmatchedMembers []string
	for _, cabPath := range cabPaths {
		unmatched, err := extractMSICab(cabPath, dest, fileMap, extracted)
		if err != nil {
			return err
		}
		unmatchedMembers = append(unmatchedMembers, unmatched...)
		unmatchedMembers = unmatchedMembers[:min(3, len(unmatchedMembers))]
	}
	if len(parsed.FileMap) > 0 && len(extracted) == 0 {
		return fmt.Errorf("MSI CABs contained none of %d File-table entries; first CAB members: %s", len(parsed.FileMap), strings.Join(unmatchedMembers, ", "))
	}
	if missing := len(parsed.FileMap) - len(extracted); missing > 0 {
		fmt.Fprintf(os.Stderr, "warning: MSI has %d file table entries not present in selected CABs\n", missing)
	}
	return nil
}

func extractMSICab(cabPath, dest string, fileMap map[string]string, extracted map[string]bool) (unmatched []string, err error) {
	cabinetReader, err := cabinet.OpenReader(cabPath)
	if errors.Is(err, cabinet.ErrAlgorithm) {
		return nil, fmt.Errorf("%s uses CAB LZX/Quantum compression, not supported by the verified pure-Go backend", filepath.Base(cabPath))
	}
	if err != nil {
		return nil, fmt.Errorf("open CAB %s: %w", filepath.Base(cabPath), err)
	}
	defer func() {
		err = errors.Join(err, cabinetReader.Close())
	}()
	for _, file := range cabinetReader.Files {
		rel, found := fileMap[strings.ToLower(file.Name)]
		if !found {
			if len(unmatched) < 3 {
				unmatched = append(unmatched, file.Name)
			}
			continue
		}
		out, err := safeJoin(dest, rel)
		if err != nil {
			return nil, err
		}
		content, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open CAB member %s: %w", file.Name, err)
		}
		if err := writeArchiveOutput(out, content, 0o644, file.Modified); err != nil {
			return nil, err
		}
		extracted[strings.ToLower(file.Name)] = true
	}
	return unmatched, nil
}

func parseMSISafely(path string) (result *winmsi.MSI, err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("MSI parser panic: %v", value)
		}
	}()
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return winmsi.Parse(file)
}

var msiNameAlphabet = []rune("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz._!")

func decodeMSIStreamName(name string) string {
	var result []rune
	for _, character := range name {
		switch {
		case character >= 0x3800 && character < 0x4800:
			result = append(result, msiNameAlphabet[(character-0x3800)&0x3f], msiNameAlphabet[((character-0x3800)>>6)&0x3f])
		case character >= 0x4800 && character <= 0x4840:
			result = append(result, msiNameAlphabet[character-0x4800])
		default:
			result = append(result, character)
		}
	}
	return string(result)
}

func extractEmbeddedMSIStream(msiPath, wanted, output string) error {
	file, err := os.Open(msiPath)
	if err != nil {
		return err
	}
	defer file.Close()
	doc, err := mscfb.New(file)
	if err != nil {
		return err
	}
	for entry, nextErr := doc.Next(); nextErr == nil; entry, nextErr = doc.Next() {
		if !strings.EqualFold(decodeMSIStreamName(entry.Name), wanted) {
			continue
		}
		return writeOutput(output, entry, 0o644, time.Time{})
	}
	return fmt.Errorf("embedded MSI stream %q not found", wanted)
}

func safeJoin(root, archivePath string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(archivePath, "\\", "/"))
	first, _, _ := strings.Cut(clean, "/")
	absolute := strings.HasPrefix(clean, "/") || strings.Contains(first, ":")
	traversal := clean == "." || clean == ".." || strings.HasPrefix(clean, "../")
	if absolute || traversal {
		return "", errors.New("unsafe archive path")
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}

func writeArchiveOutput(path string, content io.ReadCloser, mode os.FileMode, modified time.Time) error {
	return errors.Join(writeOutput(path, content, mode, modified), content.Close())
}

func writeOutput(path string, src io.Reader, mode os.FileMode, modified time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".msvcup-file-*")
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
	if _, err := io.Copy(tmp, src); err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Remove(path)
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if !modified.IsZero() {
		_ = os.Chtimes(path, modified, modified)
	}
	ok = true
	return nil
}

func copyFile(src, dest string) error {
	file, err := os.Open(src)
	if err != nil {
		return err
	}
	defer file.Close()
	return writeOutput(dest, file, 0o644, time.Time{})
}

func safeName(value string) string {
	var b strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("._-", character) {
			b.WriteRune(character)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "payload"
	}
	return b.String()
}
