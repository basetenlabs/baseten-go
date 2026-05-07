package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const (
	defaultManagementSpecURL = "https://api.baseten.co/v1/spec"
	defaultInferenceSpecURL  = "https://api.baseten.co/inference-spec"
	trussConfigSchemaURL     = "https://raw.githubusercontent.com/basetenlabs/truss/main/truss/config.schema.json"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	updateSpecs := flag.Bool("update-specs", false, "Download latest specs from remote URLs before generating")
	flag.Parse()

	_, thisFile, _, _ := runtime.Caller(0)
	apigenDir := filepath.Join(thisFile, "..")
	repoRoot := filepath.Join(thisFile, "../../../..")
	clientDir := filepath.Join(repoRoot, "client")
	specsDir := filepath.Join(apigenDir, "specs")

	managementSpecFile := filepath.Join(specsDir, "management.json")
	inferenceSpecFile := filepath.Join(specsDir, "inference.json")
	configSchemaFile := filepath.Join(specsDir, "config.schema.json")

	if *updateSpecs {
		fmt.Println("Updating specs from remote URLs...")
		if err := downloadSpecToFile(defaultManagementSpecURL, managementSpecFile); err != nil {
			return fmt.Errorf("updating management spec: %w", err)
		}
		fmt.Printf("  %s -> %s\n", defaultManagementSpecURL, managementSpecFile)
		if err := downloadSpecToFile(defaultInferenceSpecURL, inferenceSpecFile); err != nil {
			return fmt.Errorf("updating inference spec: %w", err)
		}
		fmt.Printf("  %s -> %s\n", defaultInferenceSpecURL, inferenceSpecFile)
		if err := downloadSpecToFile(trussConfigSchemaURL, configSchemaFile); err != nil {
			return fmt.Errorf("updating truss config schema: %w", err)
		}
		fmt.Printf("  %s -> %s\n", trussConfigSchemaURL, configSchemaFile)
	}

	if err := generateAPI(apigenDir, managementSpecFile, clientDir, "managementapi"); err != nil {
		return fmt.Errorf("generating management API: %w", err)
	}
	if err := generateAPI(apigenDir, inferenceSpecFile, clientDir, "inferenceapi"); err != nil {
		return fmt.Errorf("generating inference API: %w", err)
	}
	if err := generateModelConfig(apigenDir, configSchemaFile, clientDir); err != nil {
		return fmt.Errorf("generating modelconfig: %w", err)
	}
	return nil
}

func generateModelConfig(apigenDir, schemaFile, clientDir string) error {
	const pkgName = "modelconfig"
	fmt.Printf("Generating %s from %s\n", pkgName, schemaFile)
	outDir := filepath.Join(clientDir, pkgName)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	data, err := os.ReadFile(schemaFile)
	if err != nil {
		return err
	}
	data, err = preprocessConfigSchema(data)
	if err != nil {
		return fmt.Errorf("preprocessing config schema: %w", err)
	}
	tmp, err := os.CreateTemp("", "apigen-config-schema-*.json")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	outFile := filepath.Join(outDir, pkgName+".gen.go")
	cmd := exec.Command("go", "tool", "go-jsonschema",
		"--only-models",
		"--struct-name-from-title",
		"--tags", "json,yaml",
		"--package", pkgName,
		"--output", outFile,
		tmp.Name(),
	)
	cmd.Dir = apigenDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running go-jsonschema: %w", err)
	}
	src, err := os.ReadFile(outFile)
	if err != nil {
		return fmt.Errorf("reading generated file: %w", err)
	}
	src, err = postProcessModelConfig(src)
	if err != nil {
		return fmt.Errorf("post-processing modelconfig: %w", err)
	}
	if err := os.WriteFile(outFile, src, 0o644); err != nil {
		return err
	}
	fmt.Printf("  -> %s\n", outFile)
	return nil
}

func generateAPI(apigenDir, specSource, clientDir, pkgName string) error {
	fmt.Printf("Generating %s from %s\n", pkgName, specSource)
	outDir := filepath.Join(clientDir, pkgName)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	specData, cleanup, err := resolveSpec(specSource)
	if err != nil {
		return err
	}
	defer cleanup()

	// Generate models via oapi-codegen.
	modelsFile := filepath.Join(outDir, pkgName+".gen.go")
	if err := runOapiCodegen(apigenDir, specData.tmpFile, modelsFile, pkgName); err != nil {
		return err
	}
	src, err := os.ReadFile(modelsFile)
	if err != nil {
		return fmt.Errorf("reading generated file: %w", err)
	}
	src, err = postProcess(src)
	if err != nil {
		return fmt.Errorf("post-processing: %w", err)
	}
	if err := os.WriteFile(modelsFile, src, 0o644); err != nil {
		return err
	}
	fmt.Printf("  -> %s\n", modelsFile)

	// Generate client via our codegen.
	clientFile := filepath.Join(outDir, "client.gen.go")
	if err := generateClient(specData.preprocessed, clientFile, pkgName); err != nil {
		return fmt.Errorf("generating client: %w", err)
	}
	fmt.Printf("  -> %s\n", clientFile)

	return nil
}

type resolvedSpec struct {
	preprocessed []byte // JSON bytes after preprocessing
	tmpFile      string // temp file path for oapi-codegen
}

// resolveSpec reads and preprocesses a spec file, returning the preprocessed
// bytes and a temp file for tools that need a file path.
func resolveSpec(source string) (*resolvedSpec, func(), error) {
	noop := func() {}

	data, err := os.ReadFile(source)
	if err != nil {
		return nil, noop, err
	}

	data, err = preprocessSpec(data)
	if err != nil {
		return nil, noop, err
	}

	tmp, err := os.CreateTemp("", "apigen-spec-*.json")
	if err != nil {
		return nil, noop, fmt.Errorf("creating temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, noop, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return nil, noop, err
	}
	return &resolvedSpec{preprocessed: data, tmpFile: tmp.Name()}, func() { os.Remove(tmp.Name()) }, nil
}

func downloadSpecToFile(url, destFile string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: status %d", url, resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response from %s: %w", url, err)
	}
	return os.WriteFile(destFile, data, 0o644)
}

func runOapiCodegen(apigenDir, specFile, outFile, pkgName string) error {
	cmd := exec.Command("go", "tool", "oapi-codegen",
		"-config", filepath.Join(apigenDir, pkgName+".cfg.yaml"),
		"-o", outFile,
		specFile,
	)
	cmd.Dir = apigenDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
