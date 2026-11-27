package web

import (
	_ "embed"
	"encoding/json"
)

//go:embed openapi.json
var archiveOpenAPI json.RawMessage

func openAPI() json.RawMessage { return archiveOpenAPI }
