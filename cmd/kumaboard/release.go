package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jhyoong/KumaBoard/server/store"
)

func runReleaseIngest(args []string) error {
	fs := flag.NewFlagSet("release ingest", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	version := fs.String("version", "", "release version to ingest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *version == "" {
		return fmt.Errorf("usage: kumaboard release ingest --version x.y.z [-config PATH]")
	}
	cfg, err := loadConfigFlag([]string{"-config", *cfgPath})
	if err != nil {
		return err
	}
	mfPath := filepath.Join(cfg.DataDir, "releases", *version, "manifest.json")
	b, err := os.ReadFile(mfPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var mf manifest
	if err := json.Unmarshal(b, &mf); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	for _, a := range mf.Artifacts {
		if a.Signature == "" {
			return fmt.Errorf("artifact %s_%s has no signature; run kumaboard sign first", a.OS, a.Arch)
		}
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "kumaboard.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	for _, a := range mf.Artifacts {
		if err := st.IngestRelease(ctx, mf.Version, a.OS, a.Arch, a.SHA256, a.Signature, a.SizeBytes); err != nil {
			return fmt.Errorf("ingest %s_%s: %w", a.OS, a.Arch, err)
		}
		st.Audit(ctx, "cli", "release_ingest", mf.Version, "ok", a.OS+"_"+a.Arch)
		fmt.Printf("ingested %s %s_%s\n", mf.Version, a.OS, a.Arch)
	}
	return nil
}
