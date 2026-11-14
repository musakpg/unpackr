package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

const directcrateBrandSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 108 108" fill="none">
<rect width="108" height="108" rx="24" fill="#09090B"/>
<path d="M54 28 L76 40.5 L76 65.5 L54 78 L32 65.5 L32 40.5 Z" stroke="#FAFAFA" stroke-width="4.5" stroke-linecap="round" stroke-linejoin="round"/>
<path d="M32 40.5 L54 53 L76 40.5 M54 53 L54 78" stroke="#FAFAFA" stroke-width="4.5" stroke-linecap="round" stroke-linejoin="round"/>
<path d="M54 35 L54 45 M49 41 L54 46 L59 41" stroke="#10B981" stroke-width="4.2" stroke-linecap="round" stroke-linejoin="round"/>
</svg>
`

func main() {
	repoRoot, err := filepath.Abs(".")
	if err != nil {
		log.Fatalf("failed to get absolute path: %v", err)
	}

	// 1. Ensure directcrate.svg exists in repository root
	svgPath := filepath.Join(repoRoot, "directcrate.svg")
	if err := os.WriteFile(svgPath, []byte(directcrateBrandSVG), 0644); err != nil {
		log.Fatalf("failed to write directcrate.svg: %v", err)
	}
	fmt.Printf("[OK] Wrote %s\n", svgPath)

	icoPath := filepath.Join(repoRoot, "directcrate.ico")
	sysoPath := filepath.Join(repoRoot, "rsrc_windows_amd64.syso")
	webDir := filepath.Join(repoRoot, "web")
	_ = os.MkdirAll(webDir, 0755)
	webFaviconPath := filepath.Join(webDir, "favicon.ico")
	webIconPath := filepath.Join(webDir, "icon.png")

	// 2. Locate ImageMagick binary (magick or convert)
	magickCmd := "magick"
	if _, err := exec.LookPath("magick"); err != nil {
		if _, err := exec.LookPath("convert"); err == nil {
			magickCmd = "convert"
		} else {
			if _, statErr := os.Stat(sysoPath); statErr == nil {
				fmt.Println("[WARN] ImageMagick not found; preserving existing pre-generated rsrc_windows_amd64.syso and icons.")
				return
			}
			log.Fatalf("ImageMagick (magick or convert) is required to rasterize icons")
		}
	}

	// 3. Generate multi-resolution directcrate.ico (256, 128, 64, 48, 32, 16)
	cmdICO := exec.Command(magickCmd, svgPath, "-background", "none", "-define", "icon:auto-resize=256,128,64,48,32,16", icoPath)
	if out, err := cmdICO.CombinedOutput(); err != nil {
		log.Fatalf("failed to generate %s: %v\n%s", icoPath, err, string(out))
	}
	fmt.Printf("[OK] Generated %s\n", icoPath)

	// Copy to web/favicon.ico for embedded web server
	icoData, err := os.ReadFile(icoPath)
	if err != nil {
		log.Fatalf("failed to read %s: %v", icoPath, err)
	}
	if err := os.WriteFile(webFaviconPath, icoData, 0644); err != nil {
		log.Fatalf("failed to write %s: %v", webFaviconPath, err)
	}
	fmt.Printf("[OK] Generated %s\n", webFaviconPath)

	// Generate 256x256 raster icon for web/icon.png
	cmdWebPNG := exec.Command(magickCmd, "-background", "none", "-density", "300", svgPath, "-resize", "256x256", webIconPath)
	if out, err := cmdWebPNG.CombinedOutput(); err != nil {
		log.Fatalf("failed to generate %s: %v\n%s", webIconPath, err, string(out))
	}
	fmt.Printf("[OK] Generated %s\n", webIconPath)

	// 4. Generate Windows PE resource .syso file (rsrc_windows_amd64.syso)
	cmdRsrc := exec.Command("go", "run", "github.com/akavel/rsrc@latest", "-ico", icoPath, "-arch", "amd64", "-o", sysoPath)
	if out, err := cmdRsrc.CombinedOutput(); err != nil {
		log.Fatalf("failed to generate %s: %v\n%s", sysoPath, err, string(out))
	}
	fmt.Printf("[OK] Generated %s\n", sysoPath)

	// 5. Generate Android fallback mipmap PNG icons
	mipmaps := []struct {
		folder string
		size   int
	}{
		{"mipmap-mdpi", 48},
		{"mipmap-hdpi", 72},
		{"mipmap-xhdpi", 96},
		{"mipmap-xxhdpi", 144},
		{"mipmap-xxxhdpi", 192},
	}

	resBase := filepath.Join(repoRoot, "android", "app", "src", "main", "res")
	for _, m := range mipmaps {
		targetDir := filepath.Join(resBase, m.folder)
		_ = os.MkdirAll(targetDir, 0755)

		launcherPNG := filepath.Join(targetDir, "ic_launcher.png")
		roundPNG := filepath.Join(targetDir, "ic_launcher_round.png")

		sizeArg := fmt.Sprintf("%dx%d", m.size, m.size)
		cmdPNG := exec.Command(magickCmd, "-background", "none", "-density", "300", svgPath, "-resize", sizeArg, launcherPNG)
		if out, err := cmdPNG.CombinedOutput(); err != nil {
			log.Fatalf("failed to generate %s: %v\n%s", launcherPNG, err, string(out))
		}

		// Also write round icon (for legacy pre-v26 launchers)
		cmdRound := exec.Command(magickCmd, "-background", "none", "-density", "300", svgPath, "-resize", sizeArg, roundPNG)
		if out, err := cmdRound.CombinedOutput(); err != nil {
			log.Fatalf("failed to generate %s: %v\n%s", roundPNG, err, string(out))
		}
		fmt.Printf("[OK] Generated Android icons in %s (%dx%d)\n", m.folder, m.size, m.size)
	}

	fmt.Println("[SUCCESS] All native application icons and Windows PE resources generated successfully.")
}
