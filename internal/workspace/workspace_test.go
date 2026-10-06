package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const deploymentID = "8bb34af2-396c-4b37-8905-1b93c6677a1d"

func TestStoreWritesDeploymentWorkspace(t *testing.T) {
	root := t.TempDir()
	store := NewLocalStore(root)
	files := Files{
		AppFilename:          "from fastapi import FastAPI\napp = FastAPI()\n",
		RequirementsFilename: "fastapi\nuvicorn\n",
	}

	if err := store.Store(context.Background(), deploymentID, files); err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	app, err := os.ReadFile(filepath.Join(root, deploymentID, AppFilename))
	if err != nil || string(app) != files[AppFilename] {
		t.Fatalf("app.py = %q, %v", app, err)
	}
	requirements, err := os.ReadFile(filepath.Join(root, deploymentID, RequirementsFilename))
	if err != nil || string(requirements) != files[RequirementsFilename] {
		t.Fatalf("requirements.txt = %q, %v", requirements, err)
	}
}

func TestValidateFilesRejectsUnexpectedFilename(t *testing.T) {
	err := ValidateFiles(Files{
		AppFilename:          "app = object()",
		RequirementsFilename: "",
		"../secret.txt":      "not allowed",
	})
	if err == nil {
		t.Fatal("ValidateFiles() error = nil, want error")
	}
}

func TestStoreRetryIsImmutable(t *testing.T) {
	root := t.TempDir()
	store := NewLocalStore(root)
	files := Files{AppFilename: "app = 1", RequirementsFilename: ""}
	for i := 0; i < 2; i++ {
		if err := store.Store(context.Background(), deploymentID, files); err != nil {
			t.Fatal(err)
		}
	}
	files[AppFilename] = "app = 2"
	if err := store.Store(context.Background(), deploymentID, files); err == nil {
		t.Fatal("source overwrite accepted")
	}
	path := filepath.Join(root, deploymentID, AppFilename)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, deploymentID, RequirementsFilename), path); err != nil {
		t.Fatal(err)
	}
	if err := store.Store(context.Background(), deploymentID, files); err == nil {
		t.Fatal("source symlink accepted")
	}
}
