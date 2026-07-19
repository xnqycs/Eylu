package main

import (
	"reflect"
	"testing"
)

func TestClassifyLicense(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "apache",
			text: "Apache License\nVersion 2.0, January 2004",
			want: []string{"Apache-2.0"},
		},
		{
			name: "mit",
			text: "Permission is hereby granted, free of charge, to any person obtaining a copy",
			want: []string{"MIT"},
		},
		{
			name: "mixed transition",
			text: "Apache License\nVersion 2.0, January 2004\nPermission is hereby granted, free of charge, to any person obtaining a copy\nCreative Commons Attribution 4.0 International",
			want: []string{"Apache-2.0", "MIT", "CC-BY-4.0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyLicense(tt.text); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("classifyLicense() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsNoticeFile(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"LICENSE":       true,
		"LICENSE.txt":   true,
		"LICENCE.md":    true,
		"COPYING":       true,
		"NOTICE":        true,
		"PATENTS":       true,
		"copyright.txt": true,
		"license.go":    false,
		"README.md":     false,
	}
	for name, want := range tests {
		if got := isNoticeFile(name); got != want {
			t.Errorf("isNoticeFile(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestNormalizeTargets(t *testing.T) {
	t.Parallel()

	got := normalizeTargets([]string{"windows/amd64", "linux/arm64", "windows/amd64"})
	want := []string{"linux/arm64", "windows/amd64"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeTargets() = %v, want %v", got, want)
	}
}
