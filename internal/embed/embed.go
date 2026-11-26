// Package embed is the embedding runtime: adapters over llama.cpp. The batch
// embedder runs llama-embedding once per batch with Record text on stdin;
// device resolution turns the stored embed.device match into the runtime's
// -dev argument at each use; the fingerprint names the model, prompt
// template, backend and dimension a vector was made with. Decisions: #16,
// #19, #20 and its amendment, ADR-0003.
package embed

// Setup is what `embed --on` checks and saves: the runtime, the model file
// and the stable device match.
type Setup struct {
	Runtime string // path to llama.cpp's llama-embedding
	Model   string // path to the GGUF model file
	Device  string // a device name, a PCI address or DeviceCPU
}

// The model card's prompt prefixes (#19). They are part of a vector's
// identity: TemplateVersion names them.
const (
	// DocumentPrefix starts every Record document the batch embedder sends.
	DocumentPrefix = "title: none | text: "
	// QueryPrefix starts every query the warm server embeds (#43).
	QueryPrefix = "task: search result | query: "
)

// logVerbosity is the -lv at which the runtime logs the `using device` line
// that device checks read (it prints no prompt text at this level).
const logVerbosity = "4"
