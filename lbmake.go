package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	Name    string
	Version string
	Sources []string
	Deps    []string
}

type Cache map[string]string

var (
	globalCache = make(Cache)
	cacheMutex  sync.Mutex
	cachePath   = ".buildCache"
	configPath  = "build.lbconfig"
	activeTasks sync.WaitGroup
	isWatching  bool // Global tracker for watch mode
)

func main() {
	// 1. Handle Signal Interruption (Ctrl+C)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\n[LBUILD] Stop request received. Finishing active tasks...")
		done := make(chan struct{})
		go func() { activeTasks.Wait(); close(done) }()
		select {
		case <-done:
			fmt.Println("[LBUILD] Safe shutdown.")
		case <-time.After(3 * time.Second):
			fmt.Println("[LBUILD] Shutdown timeout. Force exiting...")
		}
		os.Exit(0)
	}()

	// 2. Parse Command Flags
	for _, arg := range os.Args {
		switch arg {
		case "--watch":
			isWatching = true
		case "--clean":
			cleanProject(false)
			return
		case "--cleanDependency":
			cleanProject(true)
			return
		case "--help", "-h":
			printHelp()
			return
		}
	}

	// 3. Initial Build Run
	globalCache = loadCache(cachePath)
	runBuildCycle()

	if isWatching {
		fmt.Println("[WATCH] Monitoring changes... Press Ctrl+C to stop.")
		for {
			time.Sleep(500 * time.Millisecond)
			if checkAnyChanges() {
				runBuildCycle()
			}
		}
	}
}

func printHelp() {
	fmt.Println("LBUILD - Luabasc Build System")
	fmt.Println("\nUsage:")
	fmt.Println("  ./lbuild [flags]")
	fmt.Println("\nFlags:")
	fmt.Println("  --watch            Monitor files and rebuild automatically")
	fmt.Println("  --clean            Remove binary and build cache")
	fmt.Println("  --cleanDependency  Remove binary, cache, and dependencies/ folder")
	fmt.Println("  --help, -h         Show this help menu")
}

func cleanProject(includeDeps bool) {
	fmt.Println("[CLEAN] Cleaning project files...")
	cfg := parseConfig(configPath)
	os.Remove(cachePath)
	if cfg.Name != "" { os.Remove(cfg.Name) }
	if includeDeps { os.RemoveAll("dependencies") }
	fmt.Println("[SUCCESS] Project is clean.")
}

func runBuildCycle() {
	cfg := parseConfig(configPath)
	if cfg.Name == "" {
		if !isWatching { fmt.Println("[-] Error: Could not read build.lbconfig") }
		return
	}

	cwd, _ := os.Getwd()
	depDir := filepath.Join(cwd, "dependencies")
	os.MkdirAll(depDir, 0755)

	// 1. Parallel Dependency Fetching
	var wgDeps sync.WaitGroup
	for _, dep := range cfg.Deps {
		if strings.HasSuffix(dep, ".lb") {
			wgDeps.Add(1)
			activeTasks.Add(1)
			go func(d string) {
				defer wgDeps.Done()
				defer activeTasks.Done()
				targetPath := filepath.Join(depDir, d)
				if _, err := os.Stat(targetPath); os.IsNotExist(err) {
					fmt.Printf("[FOUNDATION] Installing %s...\n", d)
					exec.Command("foundation", "install", d).Run()
				}
			}(dep)
		}
	}
	wgDeps.Wait()

	// 2. Parallel SHA256 Hashing
	needsRebuild := false
	var wgHash sync.WaitGroup
	files := append([]string{}, cfg.Sources...)
	files = append(files, configPath)
	for _, d := range cfg.Deps {
		files = append(files, filepath.Join("dependencies", d))
	}

	for _, file := range files {
		wgHash.Add(1)
		go func(f string) {
			defer wgHash.Done()
			h, err := hashFile(f)
			if err != nil { return }
			cacheMutex.Lock()
			if globalCache[f] != h {
				needsRebuild = true
				globalCache[f] = h
				fmt.Printf("[*] Change detected: %s\n", f)
			}
			cacheMutex.Unlock()
		}(file)
	}
	wgHash.Wait()

	// 3. Compilation
	if needsRebuild {
		activeTasks.Add(1)
		compilerPath, err := exec.LookPath("luabasc")
		if err != nil {
			fmt.Println("[-] Error: 'luabasc' not found in PATH.")
			activeTasks.Done()
			return
		}
		
		absCompiler, _ := filepath.Abs(compilerPath)
		entryFile := cfg.Sources[0]
		absEntry, _ := filepath.Abs(entryFile)
		sourceDir := filepath.Dir(absEntry)
		sourceBase := filepath.Base(absEntry)
		absDepDir, _ := filepath.Abs(depDir)

		args := []string{sourceBase, cfg.Name, fmt.Sprintf("-l%s", absDepDir)}
		for _, s := range cfg.Sources[1:] {
			absS, _ := filepath.Abs(s)
			args = append(args, absS)
		}
		for _, d := range cfg.Deps {
			args = append(args, filepath.Join(absDepDir, d))
		}
		args = append(args, "--shut")

		cmd := exec.Command(absCompiler, args...)
		cmd.Dir = sourceDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err == nil {
			generatedBin := filepath.Join(sourceDir, cfg.Name)
			finalBin := filepath.Join(cwd, cfg.Name)
			if _, err := os.Stat(generatedBin); err == nil && generatedBin != finalBin {
				os.Rename(generatedBin, finalBin)
			}
			saveCache(cachePath, globalCache)
			fmt.Printf("[SUCCESS] Build complete: %s\n", time.Now().Format("15:04:05"))
		} else {
			fmt.Printf("[-] Build Failed: %v\n", err)
		}
		activeTasks.Done()
	} else {
		// Only print SKIP if we are NOT in watch mode
		if !isWatching {
			fmt.Println("[SKIP] Up to date.")
		}
	}
}

func checkAnyChanges() bool {
	changed := false
	cfg := parseConfig(configPath)
	
	cacheMutex.Lock()
	defer cacheMutex.Unlock()

	filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || changed { return nil }
		if path == cachePath || path == cfg.Name || strings.HasPrefix(path, "dependencies") {
			return nil
		}
		ext := filepath.Ext(path)
		if ext == ".lb" || ext == ".lh" || ext == ".h" || path == configPath {
			h, _ := hashFile(path)
			if globalCache[path] != h {
				changed = true
			}
		}
		return nil
	})
	return changed
}

// --- Utilities ---

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil { return "", err }
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil { return "", err }
	return hex.EncodeToString(h.Sum(nil)), nil
}

func parseConfig(path string) Config {
	file, err := os.Open(path)
	if err != nil { return Config{} }
	defer file.Close()
	var cfg Config
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.Contains(line, ":") { continue }
		parts := strings.SplitN(line, ":", 2)
		key, val := strings.ToLower(strings.TrimSpace(parts[0])), strings.TrimSpace(parts[1])
		switch key {
		case "name", "project_name": cfg.Name = val
		case "sources": cfg.Sources = splitCSV(val)
		case "dependencies": cfg.Deps = splitCSV(val)
		}
	}
	return cfg
}

func splitCSV(val string) []string {
	res := strings.Split(val, ",")
	var out []string
	for _, i := range res {
		if t := strings.TrimSpace(i); t != "" { out = append(out, t) }
	}
	return out
}

func loadCache(path string) Cache {
	c := make(Cache)
	file, err := os.Open(path)
	if err != nil { return c }
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		f := strings.Fields(scanner.Text())
		if len(f) == 2 { c[f[0]] = f[1] }
	}
	return c
}

func saveCache(path string, c Cache) {
	file, _ := os.Create(path)
	defer file.Close()
	for k, v := range c { fmt.Fprintf(file, "%s %s\n", k, v) }
}