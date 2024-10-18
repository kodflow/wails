package commands

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/pterm/pterm"
	"github.com/wailsapp/wails/v3/internal/s"
)

//go:embed linuxdeploy-plugin-gtk.sh
var gtkPlugin []byte

func log(p *pterm.ProgressbarPrinter, message string) {
	p.UpdateTitle(message)
	pterm.Info.Println(message)
	p.Increment()
}

type GenerateAppImageOptions struct {
	Binary      string `description:"The binary to package including path"`
	Icon        string `description:"Path to the icon"`
	DesktopFile string `description:"Path to the desktop file"`
	OutputDir   string `description:"Path to the output directory" default:"."`
	BuildDir    string `description:"Path to the build directory"`
}

func GenerateAppImage(options *GenerateAppImageOptions) error {
	DisableFooter = true

	defer func() {
		_ = pterm.DefaultSpinner.Stop()
	}()

	if options.Binary == "" {
		return fmt.Errorf("binary not provided")
	}
	if options.Icon == "" {
		return fmt.Errorf("icon path not provided")
	}
	if options.DesktopFile == "" {
		return fmt.Errorf("desktop file path not provided")
	}
	if options.BuildDir == "" {
		// Create temp directory
		var err error
		options.BuildDir, err = os.MkdirTemp("", "wails-appimage-*")
		if err != nil {
			return err
		}
	}
	var err error
	options.OutputDir, err = filepath.Abs(options.OutputDir)
	if err != nil {
		return err
	}

	pterm.Println(pterm.LightYellow("AppImage Generator v1.0.0"))

	return generateAppImage(options)
}

func generateAppImage(options *GenerateAppImageOptions) error {
	numberOfSteps := 5
	p, _ := pterm.DefaultProgressbar.WithTotal(numberOfSteps).WithTitle("Generating AppImage").Start()

	// Get the last path of the binary and normalise the name
	name := normaliseName(filepath.Base(options.Binary))

	// Detect system architecture
	arch := runtime.GOARCH
	var appDir string
	if arch == "arm64" {
		appDir = filepath.Join(options.BuildDir, name+"-arm64.AppDir")
	} else {
		appDir = filepath.Join(options.BuildDir, name+"-x86_64.AppDir")
	}
	s.RMDIR(appDir)

	log(p, "Preparing AppImage Directory: "+appDir)

	usrBin := filepath.Join(appDir, "usr", "bin")
	s.MKDIR(options.BuildDir)
	s.MKDIR(usrBin)
	s.COPY(options.Binary, usrBin)
	s.CHMOD(filepath.Join(usrBin, filepath.Base(options.Binary)), 0755)
	dotDirIcon := filepath.Join(appDir, ".DirIcon")
	s.COPY(options.Icon, dotDirIcon)
	iconLink := filepath.Join(appDir, filepath.Base(options.Icon))
	s.DELETE(iconLink)
	s.SYMLINK(".DirIcon", iconLink)
	s.COPY(options.DesktopFile, appDir)

	// Download linuxdeploy and make it executable
	s.CD(options.BuildDir)

	// Download necessary files
	log(p, "Downloading AppImage tooling")
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		var linuxdeployURL string
		if arch == "arm64" {
			linuxdeployURL = "https://github.com/linuxdeploy/linuxdeploy/releases/download/continuous/linuxdeploy-aarch64.AppImage"
		} else {
			linuxdeployURL = "https://github.com/linuxdeploy/linuxdeploy/releases/download/continuous/linuxdeploy-x86_64.AppImage"
		}

		if !s.EXISTS(filepath.Join(options.BuildDir, filepath.Base(linuxdeployURL))) {
			s.DOWNLOAD(linuxdeployURL, filepath.Join(options.BuildDir, filepath.Base(linuxdeployURL)))
		}
		s.CHMOD(filepath.Join(options.BuildDir, filepath.Base(linuxdeployURL)), 0755)
		wg.Done()
	}()

	go func() {
		var appRunURL string
		if arch == "arm64" {
			appRunURL = "https://github.com/AppImage/AppImageKit/releases/download/continuous/AppRun-aarch64"
		} else {
			appRunURL = "https://github.com/AppImage/AppImageKit/releases/download/continuous/AppRun-x86_64"
		}

		target := filepath.Join(appDir, "AppRun")
		if !s.EXISTS(target) {
			s.DOWNLOAD(appRunURL, target)
		}
		s.CHMOD(target, 0755)
		wg.Done()
	}()

	wg.Wait()

	log(p, "Processing GTK files.")
	filesNeeded := []string{"WebKitWebProcess", "WebKitNetworkProcess", "libwebkit2gtkinjectedbundle.so"}
	files, err := findGTKFiles(filesNeeded)
	if err != nil {
		return err
	}
	s.CD(appDir)
	for _, file := range files {
		targetDir := filepath.Dir(file)
		// Strip leading forward slash
		if targetDir[0] == '/' {
			targetDir = targetDir[1:]
		}
		var err error
		targetDir, err = filepath.Abs(targetDir)
		if err != nil {
			return err
		}
		s.MKDIR(targetDir)
		s.COPY(file, targetDir)
	}

	// Copy GTK Plugin
	err = os.WriteFile(filepath.Join(options.BuildDir, "linuxdeploy-plugin-gtk.sh"), gtkPlugin, 0755)
	if err != nil {
		return err
	}

	// Determine GTK Version
	targetBinary := filepath.Join(appDir, "usr", "bin", options.Binary)
	lddOutput, err := s.EXEC(fmt.Sprintf("ldd %s", targetBinary))
	if err != nil {
		println(string(lddOutput))
		return err
	}
	lddString := string(lddOutput)
	var DeployGtkVersion string
	if s.CONTAINS(lddString, "libgtk-x11-2.0.so") {
		DeployGtkVersion = "2"
	} else if s.CONTAINS(lddString, "libgtk-3.so") {
		DeployGtkVersion = "3"
	} else if s.CONTAINS(lddString, "libgtk-4.so") {
		DeployGtkVersion = "4"
	} else {
		return fmt.Errorf("unable to determine GTK version")
	}

	// Run linuxdeploy to bundle the application
	s.CD(options.BuildDir)
	var linuxdeployAppImage string
	if arch == "arm64" {
		linuxdeployAppImage = "linuxdeploy-aarch64.AppImage"
	} else {
		linuxdeployAppImage = "linuxdeploy-x86_64.AppImage"
	}

	cmd := fmt.Sprintf("%s --appimage-extract-and-run --appdir %s --output appimage --plugin gtk", filepath.Join(options.BuildDir, linuxdeployAppImage), appDir)
	s.SETENV("DEPLOY_GTK_VERSION", DeployGtkVersion)
	fmt.Println("Running: " + cmd)
	fmt.Println("DEPLOY_GTK_VERSION", DeployGtkVersion)
	output, err := s.EXEC(cmd)
	if err != nil {
		println(output)
		return err
	}

	// Move file to output directory
	var targetFile string
	if arch == "arm64" {
		targetFile = filepath.Join(options.BuildDir, name+"-aarch64.AppImage")
	} else {
		targetFile = filepath.Join(options.BuildDir, name+"-x86_64.AppImage")
	}

	s.MOVE(targetFile, options.OutputDir)

	log(p, "AppImage created: "+targetFile)
	return nil
}

func findGTKFiles(files []string) ([]string, error) {
	notFound := []string{}
	found := []string{}
	err := filepath.Walk("/usr/", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsPermission(err) {
				return nil
			}
			return err
		}

		if info.IsDir() {
			return nil
		}

		for _, fileName := range files {
			if strings.HasSuffix(path, fileName) {
				found = append(found, path)
				break
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, fileName := range files {
		fileFound := false
		for _, foundPath := range found {
			if strings.HasSuffix(foundPath, fileName) {
				fileFound = true
				break
			}
		}
		if !fileFound {
			notFound = append(notFound, fileName)
		}
	}
	if len(notFound) > 0 {
		return nil, errors.New("Unable to locate all required files: " + strings.Join(notFound, ", "))
	}
	return found, nil
}
