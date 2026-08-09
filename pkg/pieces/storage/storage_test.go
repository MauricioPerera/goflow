package storage_test

import (
	"testing"

	"goflow/pkg/piece"
	storagepiece "goflow/pkg/pieces/storage"
)

func TestStorage_WriteText(t *testing.T) {
	p := storagepiece.New()
	act := p.Actions["write"]
	writer := piece.NewMemoryFileWriter()

	out, err := act.Run(piece.ActionContext{
		Input: map[string]any{"fileName": "hello.txt", "content": "hello world", "format": "text"},
		Files: writer,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	url, _ := out.(map[string]any)["fileURL"].(string)
	if url == "" {
		t.Fatal("fileURL is empty")
	}
	data, ok := writer.Get(url)
	if !ok || string(data) != "hello world" {
		t.Fatalf("stored data = %q, ok=%v, want %q", data, ok, "hello world")
	}
}

func TestStorage_WriteBase64(t *testing.T) {
	p := storagepiece.New()
	act := p.Actions["write"]
	writer := piece.NewMemoryFileWriter()

	// "hello world" base64-encoded.
	const b64 = "aGVsbG8gd29ybGQ="
	out, err := act.Run(piece.ActionContext{
		Input: map[string]any{"fileName": "hello.bin", "content": b64, "format": "base64"},
		Files: writer,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	url, _ := out.(map[string]any)["fileURL"].(string)
	data, ok := writer.Get(url)
	if !ok || string(data) != "hello world" {
		t.Fatalf("stored data = %q, ok=%v, want decoded %q", data, ok, "hello world")
	}
}

func TestStorage_InvalidBase64FailsClearly(t *testing.T) {
	p := storagepiece.New()
	act := p.Actions["write"]

	_, err := act.Run(piece.ActionContext{
		Input: map[string]any{"fileName": "x", "content": "not-valid-base64!!", "format": "base64"},
		Files: piece.NewMemoryFileWriter(),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a base64-decoding error")
	}
}

func TestStorage_UnknownFormatFailsClearly(t *testing.T) {
	p := storagepiece.New()
	act := p.Actions["write"]

	_, err := act.Run(piece.ActionContext{
		Input: map[string]any{"fileName": "x", "content": "y", "format": "yaml"},
		Files: piece.NewMemoryFileWriter(),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a rejection for an unknown format")
	}
}

func TestStorage_MissingFileNameFailsClearly(t *testing.T) {
	p := storagepiece.New()
	act := p.Actions["write"]

	_, err := act.Run(piece.ActionContext{
		Input: map[string]any{"content": "y", "format": "text"},
		Files: piece.NewMemoryFileWriter(),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-fileName error")
	}
}

func TestStorage_MissingContentFailsClearly(t *testing.T) {
	p := storagepiece.New()
	act := p.Actions["write"]

	_, err := act.Run(piece.ActionContext{
		Input: map[string]any{"fileName": "x", "format": "text"},
		Files: piece.NewMemoryFileWriter(),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a missing-content error")
	}
}

func TestStorage_FormatDropdownReturnsExactOptions(t *testing.T) {
	p := storagepiece.New()
	dd := p.Actions["write"].Dropdowns["format"]

	state, err := dd.LoadOptions(map[string]any{}, piece.PropertyContext{})
	if err != nil {
		t.Fatalf("LoadOptions() error = %v", err)
	}
	if len(state.Options) != 2 {
		t.Fatalf("options = %+v, want exactly 2", state.Options)
	}
	if state.Options[0].Value != "text" || state.Options[1].Value != "base64" {
		t.Fatalf("options = %+v, want text then base64", state.Options)
	}
}
