package latency

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	correlationPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	identifierPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
)

// Decode strictly decodes one evidence file. Unknown fields, duplicate keys,
// missing or null fields, wrong types, trailing data and an unsupported schema
// version are structural errors. Error messages name schema paths only and
// never echo evidence content.
func Decode(data []byte) (Evidence, error) {
	if err := checkStructure(data); err != nil {
		return Evidence{}, err
	}
	var value Evidence
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Evidence{}, errors.New("invalid evidence JSON")
	}
	if err := validate(value); err != nil {
		return Evidence{}, err
	}
	return value, nil
}

func checkStructure(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walk(decoder, "$", reflect.TypeOf(Evidence{})); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("evidence must contain a single JSON value")
	}
	return nil
}

// walk checks one JSON value against the Go type it decodes into.
func walk(decoder *json.Decoder, path string, typ reflect.Type) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON at %s", path)
	}
	switch typ.Kind() {
	case reflect.Struct:
		if token != json.Delim('{') {
			return fmt.Errorf("%s must be an object", path)
		}
		return walkObject(decoder, path, typ)
	case reflect.Slice:
		if token != json.Delim('[') {
			return fmt.Errorf("%s must be an array", path)
		}
		for index := 0; decoder.More(); index++ {
			if err := walk(decoder, fmt.Sprintf("%s[%d]", path, index), typ.Elem()); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return fmt.Errorf("invalid JSON at %s", path)
		}
		return nil
	case reflect.String:
		if _, ok := token.(string); !ok {
			return fmt.Errorf("%s must be a string", path)
		}
		return nil
	case reflect.Int64:
		number, ok := token.(json.Number)
		if !ok {
			return fmt.Errorf("%s must be an integer", path)
		}
		if _, err := strconv.ParseInt(string(number), 10, 64); err != nil {
			return fmt.Errorf("%s must be an integer", path)
		}
		return nil
	case reflect.Int:
		number, ok := token.(json.Number)
		if !ok {
			return fmt.Errorf("%s must be an integer", path)
		}
		if _, err := strconv.Atoi(string(number)); err != nil {
			return fmt.Errorf("%s must be an integer", path)
		}
		return nil
	case reflect.Float64:
		number, ok := token.(json.Number)
		if !ok {
			return fmt.Errorf("%s must be a number", path)
		}
		if _, err := strconv.ParseFloat(string(number), 64); err != nil {
			return fmt.Errorf("%s must be a number", path)
		}
		return nil
	default:
		return fmt.Errorf("unsupported schema type at %s", path)
	}
}

func walkObject(decoder *json.Decoder, path string, typ reflect.Type) error {
	fields := make(map[string]reflect.Type, typ.NumField())
	names := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		fields[name] = typ.Field(i).Type
		names = append(names, name)
	}
	seen := make(map[string]bool, len(fields))
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("invalid JSON at %s", path)
		}
		key, _ := keyToken.(string)
		fieldType, known := fields[key]
		if !known {
			return fmt.Errorf("unknown field at %s", path)
		}
		if seen[key] {
			return fmt.Errorf("duplicate field at %s.%s", path, key)
		}
		seen[key] = true
		if err := walk(decoder, path+"."+key, fieldType); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("invalid JSON at %s", path)
	}
	for _, name := range names {
		if !seen[name] {
			return fmt.Errorf("missing field %s.%s", path, name)
		}
	}
	return nil
}

func validate(value Evidence) error {
	if value.SchemaVersion != 1 {
		return errors.New("schemaVersion must be 1")
	}
	transmission := value.Transmission
	if !identifierPattern.MatchString(transmission.ID) {
		return errors.New("$.transmission.id must be an identifier of letters, digits, '.', '_', ':' or '-'")
	}
	if !correlationPattern.MatchString(transmission.CorrelationID) {
		return errors.New("$.transmission.correlationId must be 32 lowercase hex")
	}
	started, err := time.Parse(time.RFC3339, transmission.StartedAt)
	if err != nil {
		return errors.New("$.transmission.startedAt must be RFC3339")
	}
	completed, err := time.Parse(time.RFC3339, transmission.CompletedAt)
	if err != nil {
		return errors.New("$.transmission.completedAt must be RFC3339")
	}
	if completed.Before(started) {
		return errors.New("$.transmission.completedAt must not precede startedAt")
	}
	for index, sample := range value.Samples {
		if !isKind(sample.Kind) {
			return fmt.Errorf("$.samples[%d].kind must be one of %s", index, strings.Join(Kinds, ", "))
		}
	}
	return nil
}

func isKind(kind string) bool {
	for _, known := range Kinds {
		if kind == known {
			return true
		}
	}
	return false
}
