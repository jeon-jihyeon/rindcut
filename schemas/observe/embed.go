package observe

import _ "embed"

//go:embed definition.schema.json
var definition string

func Definition() string { return definition }
