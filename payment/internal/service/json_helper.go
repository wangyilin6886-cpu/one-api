package service

import "encoding/json"

// jsonStdMarshal is a stable alias for encoding/json.Marshal. Service code
// goes through this so a future change (sonic, jsoniter) is a one-line edit.
func jsonStdMarshal(v any) ([]byte, error) { return json.Marshal(v) }
