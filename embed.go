package main

import (
	"embed"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed embed/*.deb
var embeddedPkgs embed.FS

//go:embed static/index.html
var indexHTML []byte

// EnsureOpenVPN extracts the embedded openvpn binary to dataDir if not found
// in PATH. Returns the path to use for openvpn invocations.
func EnsureOpenVPN(dataDir string) (binPath string, libDir string) {
	libDir = filepath.Join(dataDir, "lib")
	cached := filepath.Join(dataDir, "openvpn")

	// 1. Use system openvpn if present
	if path, err := exec.LookPath("openvpn"); err == nil {
		log.Printf("Using system openvpn: %s", path)
		os.MkdirAll(libDir, 0755)
		extractLibs(libDir)
		return path, libDir
	}

	// 2. Use cached extraction
	if info, err := os.Stat(cached); err == nil && info.Mode()&0111 != 0 {
		log.Printf("Using cached openvpn: %s", cached)
		return cached, libDir
	}

	// 3. Extract from embedded .deb files
	log.Println("Extracting embedded OpenVPN packages…")
	tmpDir, err := os.MkdirTemp("", "vpnctl-extract-*")
	if err != nil {
		log.Fatalf("mktemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	entries, err := embeddedPkgs.ReadDir("embed")
	if err != nil {
		log.Fatalf("read embedded: %v", err)
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".deb") {
			continue
		}
		data, _ := embeddedPkgs.ReadFile("embed/" + entry.Name())
		debPath := filepath.Join(tmpDir, entry.Name())
		if err := os.WriteFile(debPath, data, 0644); err != nil {
			log.Printf("write deb %s: %v", entry.Name(), err)
			continue
		}
		extractDir := filepath.Join(tmpDir, "x-"+entry.Name())
		os.MkdirAll(extractDir, 0755)
		out, err := exec.Command("dpkg-deb", "-x", debPath, extractDir).CombinedOutput()
		if err != nil {
			log.Printf("dpkg-deb %s: %v — %s", entry.Name(), err, out)
		}
	}

	// Find openvpn binary
	found := ""
	_ = filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if filepath.Base(path) == "openvpn" && info.Mode()&0111 != 0 {
			found = path
		}
		return nil
	})
	if found == "" {
		log.Fatalf("openvpn binary not found in embedded packages — make sure embed/*.deb are committed")
	}

	// Copy to data dir
	data, err := os.ReadFile(found)
	if err != nil {
		log.Fatalf("read openvpn: %v", err)
	}
	if err := os.WriteFile(cached, data, 0755); err != nil {
		log.Fatalf("write openvpn: %v", err)
	}

	// Extract libs
	os.MkdirAll(libDir, 0755)
	_ = filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if strings.HasPrefix(base, "libpkcs11") || strings.HasPrefix(base, "libssl") {
			if d, err := os.ReadFile(path); err == nil {
				os.WriteFile(filepath.Join(libDir, base), d, 0755)
			}
		}
		return nil
	})

	log.Printf("OpenVPN v2.6.9 extracted to %s", cached)
	return cached, libDir
}

// extractLibs extracts only the library files (no openvpn binary)
func extractLibs(libDir string) {
	if _, err := os.Stat(filepath.Join(libDir, "libpkcs11-helper.so.1")); err == nil {
		return // already extracted
	}
	tmpDir, err := os.MkdirTemp("", "vpnctl-lib-*")
	if err != nil {
		return
	}
	defer os.RemoveAll(tmpDir)

	entries, _ := embeddedPkgs.ReadDir("embed")
	for _, entry := range entries {
		name := entry.Name()
		if !strings.Contains(name, "libpkcs11") {
			continue
		}
		data, _ := embeddedPkgs.ReadFile("embed/" + name)
		debPath := filepath.Join(tmpDir, name)
		os.WriteFile(debPath, data, 0644)
		extractDir := filepath.Join(tmpDir, "x")
		os.MkdirAll(extractDir, 0755)
		exec.Command("dpkg-deb", "-x", debPath, extractDir).Run()
		filepath.Walk(extractDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			base := filepath.Base(path)
			if strings.HasPrefix(base, "libpkcs11") {
				if d, e := os.ReadFile(path); e == nil {
					os.WriteFile(filepath.Join(libDir, base), d, 0755)
				}
			}
			return nil
		})
	}
}

// CheckXray returns error message if xray is not available
func CheckXray() error {
	_, err := exec.LookPath("xray")
	if err != nil {
		return fmt.Errorf("xray not found in PATH — install from https://github.com/XTLS/Xray-core")
	}
	return nil
}
