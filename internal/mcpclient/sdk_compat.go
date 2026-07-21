package mcpclient

import (
	"errors"
	"reflect"
	"unsafe"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpProtocolVersion = "2025-11-25"

// go-sdk v1.7.0-pre.3 reserves protocolVersion as an unexported session option.
// Validate its complete layout so an SDK upgrade fails closed instead of
// silently negotiating a newer MCP protocol.
func pinnedClientSessionOptions() (*sdkmcp.ClientSessionOptions, error) {
	options := new(sdkmcp.ClientSessionOptions)
	optionsType := reflect.TypeOf(*options)
	stringType := reflect.TypeOf("")
	if optionsType.NumField() != 1 || optionsType.Size() != stringType.Size() {
		return nil, errors.New("MCP SDK client session options layout changed")
	}
	fieldType := optionsType.Field(0)
	if fieldType.Name != "protocolVersion" || fieldType.Type != stringType || fieldType.Offset != 0 {
		return nil, errors.New("MCP SDK protocol compatibility option changed")
	}
	field := reflect.ValueOf(options).Elem().Field(0)
	if !field.CanAddr() {
		return nil, errors.New("MCP SDK protocol compatibility option is not addressable")
	}
	reflect.NewAt(stringType, unsafe.Pointer(field.UnsafeAddr())).Elem().SetString(mcpProtocolVersion)
	return options, nil
}
