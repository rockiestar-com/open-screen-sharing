package main

import (
	"debug/buildinfo"
	"flag"
	"go/build"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"

	_ "github.com/sagernet/gomobile"
	"github.com/sagernet/sing/common"
)

const (
	gomobileModule = "github.com/sagernet/gomobile"
	iosVersion     = "15.0"
	macOSVersion   = "14.0"
	outputPath     = "Frameworks/Libsharing.xcframework"
)

func main() {
	target := flag.String("target", "ios,iossimulator,macos", "gomobile bind target")
	flag.Parse()
	binDirectory := filepath.Join(filepath.SplitList(build.Default.GOPATH)[0], "bin")
	gomobileVersion := pinnedGomobileVersion()
	var missingTools []string
	for _, tool := range []string{"gomobile", "gobind"} {
		if !toolInstalled(filepath.Join(binDirectory, tool), gomobileModule+"/cmd/"+tool, gomobileVersion) {
			missingTools = append(missingTools, gomobileModule+"/cmd/"+tool+"@"+gomobileVersion)
		}
	}
	if len(missingTools) > 0 {
		install := exec.Command("go", append([]string{"install", "-v"}, missingTools...)...)
		install.Env = append(os.Environ(), "GOBIN="+binDirectory)
		run(install)
	}

	err := os.MkdirAll(filepath.Dir(outputPath), 0o755)
	if err != nil {
		log.Fatal(err)
	}
	bind := exec.Command(filepath.Join(binDirectory, "gomobile"),
		"bind",
		"-v",
		"-target", *target,
		"-iosversion", iosVersion,
		"-macosversion", macOSVersion,
		"-trimpath",
		"-buildvcs=false",
		"-ldflags", "-s -w -buildid= -checklinkname=0",
		"-o", outputPath,
		"./libsharing",
	)
	bind.Env = append(os.Environ(), "PATH="+binDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	run(bind)
}

func pinnedGomobileVersion() string {
	info, loaded := debug.ReadBuildInfo()
	if !loaded {
		log.Fatal("missing build info")
	}
	module := common.Find(info.Deps, func(module *debug.Module) bool {
		return module.Path == gomobileModule
	})
	if module == nil {
		log.Fatal("missing ", gomobileModule, " in build info")
	}
	return module.Version
}

func toolInstalled(path string, packagePath string, version string) bool {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return false
	}
	return info.Path == packagePath && info.Main.Path == gomobileModule && info.Main.Version == version
}

func run(command *exec.Cmd) {
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	err := command.Run()
	if err != nil {
		log.Fatal(err)
	}
}
