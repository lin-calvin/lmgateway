package config

// Regenerate the embedded plan catalog from the OpenCode Go model endpoint.
// The generated JSON is committed; builds do not touch the network.
//go:generate go run ../../cmd/genplanpresets -out plan_presets.json
