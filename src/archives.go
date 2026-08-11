// Archive and installer-container handling for msvcup.
package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	winmsi "github.com/ArchangelX360/winarchive-tools/msi"
	"github.com/abemedia/go-cabinet"
	"github.com/richardlehane/mscfb"
)

func extractCabinetFiles(reader *cabinet.Reader, dest string) error {
	for _, archiveFile := range reader.Files {
		out, err := safeJoin(dest, archiveFile.Name)
		if err != nil {
			return fmt.Errorf("unsafe CAB entry %q: %w", archiveFile.Name, err)
		}
		content, err := archiveFile.Open()
		if err != nil {
			if errors.Is(err, cabinet.ErrAlgorithm) {
				return fmt.Errorf("CAB compression for %s is unsupported: %w", archiveFile.Name, err)
			}
			return err
		}
		err = writeOutput(out, content, 0o644, time.Time{})
		closeErr := content.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func installPackage(packageInfo *vsPackage, downloaded map[string]string, dest string, keepRaw bool) error {
	lookup := map[string]string{}
	for _, item := range packageInfo.Payloads {
		if path := downloaded[payloadKey(item)]; path != "" {
			lookup[strings.ToLower(payloadBaseName(item))] = path
		}
	}
	for _, item := range packageInfo.Payloads {
		path := downloaded[payloadKey(item)]
		if path == "" {
			continue
		}
		name := payloadBaseName(item)
		ext := strings.ToLower(filepath.Ext(name))
		switch ext {
		case ".msi":
			if err := extractMSI(path, dest, lookup); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		case ".vsix", ".zip", ".nupkg":
			if err := extractZipPayload(path, dest, packageInfo, ext == ".vsix"); err != nil {
				return err
			}
		case ".cab":
			// CAB payloads are consumed through the corresponding MSI file map.
		case ".exe", ".msu":
			if keepRaw {
				rawDest := filepath.Join(dest, "_payloads", safeName(packageInfo.ID), name)
				if err := copyFile(path, rawDest); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(os.Stderr, "warning: retained in cache but not executed: %s (%s)\n", name, packageInfo.ID)
			}
		default:
			if keepRaw {
				rawDest := filepath.Join(dest, "_payloads", safeName(packageInfo.ID), name)
				if err := copyFile(path, rawDest); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func extractZipPayload(archivePath, dest string, packageInfo *vsPackage, vsix bool) error {
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
	root := expandExtensionDir(dest, packageInfo.ExtensionDir)
	archivePrefix := strings.TrimLeft(strings.ReplaceAll(packageInfo.ArchivePrefix, "\\", "/"), "/")
	if packageInfo.ArchiveTarget != "" {
		var err error
		root, err = safeJoin(dest, packageInfo.ArchiveTarget)
		if err != nil {
			return fmt.Errorf("invalid archive target %q: %w", packageInfo.ArchiveTarget, err)
		}
	}
	for _, archiveFile := range archiveReader.File {
		name := strings.ReplaceAll(archiveFile.Name, "\\", "/")
		if archiveFile.FileInfo().IsDir() {
			continue
		}
		if archivePrefix != "" {
			if !strings.HasPrefix(strings.ToLower(name), strings.ToLower(archivePrefix)) {
				continue
			}
			name = name[len(archivePrefix):]
		} else if hasContents {
			if !strings.HasPrefix(strings.ToLower(name), "contents/") {
				continue
			}
			name = name[len("Contents/"):]
		} else if vsix && isVSIXMetadata(name) {
			continue
		}
		if decoded, err := url.PathUnescape(name); err == nil {
			name = decoded
		}
		out, err := safeJoin(root, name)
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
		err = writeOutput(out, content, mode, archiveFile.Modified)
		closeErr := content.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func isVSIXMetadata(name string) bool {
	lower := strings.ToLower(strings.ReplaceAll(name, "\\", "/"))
	return strings.HasPrefix(lower, "_rels/") ||
		strings.HasPrefix(lower, "package/services/") ||
		lower == "[content_types].xml" || lower == "extension.vsixmanifest" ||
		lower == "manifest.json" || lower == "catalog.json"
}

func expandExtensionDir(dest, value string) string {
	value = strings.ReplaceAll(value, "\\", "/")
	lower := strings.ToLower(value)
	for _, token := range []string{"[installdir]", "[installroot]"} {
		if strings.HasPrefix(lower, token) {
			return filepath.Join(dest, filepath.FromSlash(strings.TrimLeft(value[len(token):], "/")))
		}
	}
	if value == "" {
		return dest
	}
	return filepath.Join(dest, "_extensions", filepath.FromSlash(strings.TrimLeft(value, "/")))
}

func extractMSI(msiPath, dest string, cabLookup map[string]string) (err error) {
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
		if strings.HasPrefix(name, "#") {
			if tempDir == "" {
				tempDir, err = os.MkdirTemp("", "msvcup-embedded-cab-*")
				if err != nil {
					return err
				}
			}
			out := filepath.Join(tempDir, safeName(strings.TrimPrefix(name, "#"))+".cab")
			if err := extractEmbeddedMSIStream(msiPath, strings.TrimPrefix(name, "#"), out); err != nil {
				return err
			}
			cabPaths = append(cabPaths, out)
			continue
		}
		path := cabLookup[strings.ToLower(filepath.Base(strings.ReplaceAll(name, "\\", "/")))]
		if path == "" {
			return fmt.Errorf("MSI requires missing CAB %q", name)
		}
		cabPaths = append(cabPaths, path)
	}
	extracted := map[string]bool{}
	var unmatchedMembers []string
	for _, cabPath := range cabPaths {
		cabinetReader, err := cabinet.OpenReader(cabPath)
		if err != nil {
			if errors.Is(err, cabinet.ErrAlgorithm) {
				return fmt.Errorf("%s uses CAB LZX/Quantum compression, not supported by the verified pure-Go backend", filepath.Base(cabPath))
			}
			return fmt.Errorf("open CAB %s: %w", filepath.Base(cabPath), err)
		}
		for _, file := range cabinetReader.Files {
			rel, ok := fileMap[strings.ToLower(file.Name)]
			if !ok {
				if len(unmatchedMembers) < 3 {
					unmatchedMembers = append(unmatchedMembers, file.Name)
				}
				continue
			}
			out, err := safeJoin(dest, rel)
			if err != nil {
				cabinetReader.Close()
				return err
			}
			content, err := file.Open()
			if err != nil {
				cabinetReader.Close()
				return fmt.Errorf("open CAB member %s: %w", file.Name, err)
			}
			err = writeOutput(out, content, 0o644, file.Modified)
			closeErr := content.Close()
			if err != nil {
				cabinetReader.Close()
				return err
			}
			if closeErr != nil {
				cabinetReader.Close()
				return closeErr
			}
			extracted[strings.ToLower(file.Name)] = true
		}
		if err := cabinetReader.Close(); err != nil {
			return err
		}
	}
	if len(parsed.FileMap) > 0 && len(extracted) == 0 {
		return fmt.Errorf("MSI CABs contained none of %d File-table entries; first CAB members: %s", len(parsed.FileMap), strings.Join(unmatchedMembers, ", "))
	}
	if missing := len(parsed.FileMap) - len(extracted); missing > 0 {
		fmt.Fprintf(os.Stderr, "warning: MSI has %d file table entries not present in selected CABs\n", missing)
	}
	return nil
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
	archivePath = strings.ReplaceAll(archivePath, "\\", "/")
	archivePath = strings.TrimPrefix(archivePath, "./")
	if strings.HasPrefix(archivePath, "/") || strings.Contains(strings.SplitN(archivePath, "/", 2)[0], ":") {
		return "", errors.New("absolute path")
	}
	clean := filepath.Clean(filepath.FromSlash(archivePath))
	if clean == "." || clean == "" || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path traversal")
	}
	joined := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes output directory")
	}
	return joined, nil
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
