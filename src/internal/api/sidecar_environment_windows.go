//go:build windows

package api

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"

	"qindexer/internal/config"
)

func prepareSidecarEnvironment(log *slog.Logger, settings config.OCRConfig) {
	if !settings.Enabled {
		return
	}
	if err := refreshWindowsSystemPath(); err != nil {
		log.Warn("could not refresh service PATH from machine environment", "error", err)
	}

	for _, directory := range installedSidecarDirectories() {
		if err := addProcessPathEntry(directory); err != nil {
			log.Warn("could not add OCR sidecar directory to service PATH", "directory", directory, "error", err)
			continue
		}
		if err := addMachinePathEntry(directory); err != nil {
			log.Warn("OCR sidecar directory is usable by QIndexer but machine PATH could not be repaired", "directory", directory, "error", err)
		}
	}

	tesseract := commandAvailable(settings.TesseractCommand)
	ocrmypdf := commandAvailable(settings.OCRmyPDFCommand)
	ghostscript := commandAvailable("gswin64c") || commandAvailable("gswin32c")
	log.Info("OCR sidecar startup check", "engine", settings.Engine, "tesseract", tesseract, "ocrmypdf", ocrmypdf, "ghostscript", ghostscript)
	if (settings.Engine == "tesseract" && !tesseract) ||
		(settings.Engine == "ocrmypdf" && (!ocrmypdf || !ghostscript || !tesseract)) ||
		(settings.Engine == "auto" && (!tesseract || !ocrmypdf || !ghostscript)) {
		log.Warn("OCR is enabled but required sidecars are unavailable", "engine", settings.Engine, "tesseract", tesseract, "ocrmypdf", ocrmypdf, "ghostscript", ghostscript)
	}
}

func installedSidecarDirectories() []string {
	candidates := []string{filepath.Join(os.Getenv("ProgramFiles"), "Tesseract-OCR")}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "sidecars", "python", "Scripts"))
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "sidecars", "ghostscript", "bin"))
	}
	if matches, err := filepath.Glob(filepath.Join(os.Getenv("ProgramFiles"), "gs", "gs*", "bin")); err == nil {
		candidates = append(candidates, matches...)
	}
	directories := make([]string, 0, len(candidates))
	for _, directory := range candidates {
		if info, err := os.Stat(directory); err == nil && info.IsDir() {
			directories = append(directories, directory)
		}
	}
	return directories
}

func addProcessPathEntry(directory string) error {
	if _, err := os.Stat(directory); err != nil {
		return err
	}
	if pathContains(os.Getenv("PATH"), directory) {
		return nil
	}
	return os.Setenv("PATH", directory+";"+os.Getenv("PATH"))
}

func addMachinePathEntry(directory string) error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	current, _, err := key.GetStringValue("Path")
	if err != nil {
		return err
	}
	if pathContains(current, directory) {
		return nil
	}
	return key.SetExpandStringValue("Path", strings.TrimRight(current, ";")+";"+directory)
}

func pathContains(path, directory string) bool {
	for _, entry := range strings.Split(path, ";") {
		if strings.EqualFold(strings.TrimRight(strings.TrimSpace(entry), `\/`), strings.TrimRight(directory, `\/`)) {
			return true
		}
	}
	return false
}
