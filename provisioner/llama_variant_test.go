package provisioner

import (
	"os"
	"path/filepath"
	"testing"
)

// Mi 8 (Snapdragon 845): has fphp/asimdhp but no dotprod (asimddp).
const mi8Features = "fp asimd evtstrm aes pmull sha1 sha2 crc32 atomics fphp asimdhp"

// A modern phone with both fp16 and dotprod support.
const modernFeatures = "fp asimd evtstrm aes pmull sha1 sha2 crc32 atomics fphp asimdhp asimdrdm asimddp"

func TestParseCPUFeatures(t *testing.T) {
	cpuinfo := "processor\t: 0\n" +
		"Features\t: " + mi8Features + "\n" +
		"CPU implementer\t: 0x51\n"
	got := ParseCPUFeatures(cpuinfo)
	for _, f := range []string{"fphp", "asimdhp", "aes"} {
		if !got[f] {
			t.Errorf("expected feature %q to be set", f)
		}
	}
	if got["asimddp"] {
		t.Error("asimddp should not be set for the Mi 8 features line")
	}

	// lowercase "features" (some kernels report it lowercase) is also matched.
	got = ParseCPUFeatures("processor : 0\nfeatures : fp asimd asimddp\n")
	if !got["asimddp"] {
		t.Error("lowercase features line should be parsed")
	}

	// Only the first Features line is used.
	got = ParseCPUFeatures("Features\t: fp asimd\nFeatures\t: asimddp\n")
	if got["asimddp"] {
		t.Error("only the first Features line should be parsed")
	}
	if !got["fp"] {
		t.Error("features from the first line should be present")
	}
}

func TestSelectLlamaVariant(t *testing.T) {
	all := []string{"armv8.2-a+dotprod+fp16", "armv8.2-a+fp16", "armv8-a"}

	t.Run("Mi8 picks fp16 variant", func(t *testing.T) {
		got, err := SelectLlamaVariant(ParseCPUFeatures("Features: "+mi8Features), all)
		if err != nil || got != "armv8.2-a+fp16" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("modern phone picks dotprod variant", func(t *testing.T) {
		got, err := SelectLlamaVariant(ParseCPUFeatures("Features: "+modernFeatures), all)
		if err != nil || got != "armv8.2-a+dotprod+fp16" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("no fp16 falls back to armv8-a", func(t *testing.T) {
		got, err := SelectLlamaVariant(ParseCPUFeatures("Features: fp asimd aes pmull sha1 sha2 crc32"), all)
		if err != nil || got != "armv8-a" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("required variant not built falls back to next available", func(t *testing.T) {
		// Modern phone (qualifies for dotprod), but only armv8-a was built.
		got, err := SelectLlamaVariant(ParseCPUFeatures("Features: "+modernFeatures), []string{"armv8-a"})
		if err != nil || got != "armv8-a" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("nothing suitable errors", func(t *testing.T) {
		_, err := SelectLlamaVariant(ParseCPUFeatures("Features: "+mi8Features), nil)
		if err == nil {
			t.Fatal("expected an error when no variant is available")
		}
		t.Log(err) // eyeball the message: should mention phone features and available builds
	})
}

func TestAvailableLlamaVariants(t *testing.T) {
	dir := t.TempDir()
	mustExecutable := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustExecutable(filepath.Join(dir, "armv8.2-a+dotprod+fp16", "llama-server"))
	mustExecutable(filepath.Join(dir, "armv8-a", "llama-server"))
	// Not a variant: no llama-server binary inside.
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Not a variant: llama-server present but not executable.
	if err := os.MkdirAll(filepath.Join(dir, "not-exec"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-exec", "llama-server"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got := AvailableLlamaVariants(dir)
	want := map[string]bool{"armv8.2-a+dotprod+fp16": true, "armv8-a": true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, v := range got {
		if !want[v] {
			t.Fatalf("unexpected variant %q in %v", v, got)
		}
	}

	if got := AvailableLlamaVariants(filepath.Join(dir, "does-not-exist")); got != nil {
		t.Fatalf("expected nil for missing dir, got %v", got)
	}
}
