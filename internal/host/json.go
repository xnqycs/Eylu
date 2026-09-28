package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

// Reject duplicate keys, case-folded field aliases, missing fields and unknown
// fields, including in nested DTOs. RawMessage is reserved for schema/data.
func decode(raw []byte, target any) error {
	if !utf8.Valid(raw) {
		return fmt.Errorf("invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := uniqueJSON(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	var value any
	d = json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		return err
	}
	if err := fields(value, reflect.TypeOf(target).Elem()); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(target)
}
func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 128 {
		return fmt.Errorf("JSON nesting limit")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); ok {
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return fmt.Errorf("duplicate JSON key")
				}
				seen[name] = true
				if err := uniqueJSON(d, depth+1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := uniqueJSON(d, depth+1); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected delimiter")
		}
		_, err = d.Token()
	}
	return err
}

var rawType = reflect.TypeOf(json.RawMessage{})

func fields(value any, typ reflect.Type) error {
	if typ == rawType || typ.Kind() == reflect.Interface {
		return nil
	}
	if typ.Kind() == reflect.Pointer {
		if value == nil {
			return nil
		}
		return fields(value, typ.Elem())
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("expected object")
		}
		allowed := map[string]reflect.StructField{}
		var collect func(reflect.Type)
		collect = func(t reflect.Type) {
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				if f.Anonymous {
					collect(f.Type)
					continue
				}
				name := strings.Split(f.Tag.Get("json"), ",")[0]
				if name != "-" {
					allowed[name] = f
				}
			}
		}
		collect(typ)
		for name := range object {
			if _, ok := allowed[name]; !ok {
				return fmt.Errorf("unknown field")
			}
		}
		for name, f := range allowed {
			v, exists := object[name]
			if !exists {
				if !strings.Contains(f.Tag.Get("json"), ",omitempty") {
					return fmt.Errorf("missing field")
				}
				continue
			}
			if err := fields(v, f.Type); err != nil {
				return err
			}
		}
	case reflect.Slice:
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("expected array")
		}
		for _, v := range array {
			if err := fields(v, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("expected map")
		}
		for _, v := range object {
			if err := fields(v, typ.Elem()); err != nil {
				return err
			}
		}
	default:
		if value == nil {
			return fmt.Errorf("unexpected null")
		}
	}
	return nil
}
