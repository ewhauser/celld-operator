package controlplane

import (
	"strings"
	"testing"
	"time"
)

func TestObservationJSONValidation(t *testing.T) {
	for name, data := range map[string]string{
		"malformed":          `{`,
		"root duplicate":     `{"shutdown":null,"shutdown":null}`,
		"escaped duplicate":  `{"shutdown":null,"\u0073hutdown":null}`,
		"nested duplicate":   `{"unobserved":{"a":1,"a":2}}`,
		"census duplicate":   `{"deployment":{"cells":{"a":1,"\u0061":2}}}`,
		"array duplicate":    `{"unobserved":[{"a":1,"a":2}]}`,
		"trailing value":     `{} {}`,
		"trailing malformed": `{} garbage`,
		"array":              `[]`,
		"null":               `null`,
		"excessive depth":    `{"a":` + strings.Repeat("[", 64) + `0` + strings.Repeat("]", 64) + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validObject([]byte(data)); err == nil {
				t.Fatal("accepted invalid JSON")
			}
			if _, err := decodeState([]byte(data)); err == nil {
				t.Fatal("state accepted invalid JSON")
			}
			if _, err := decodeApplication([]byte(data), time.Now()); err == nil {
				t.Fatal("application accepted invalid JSON")
			}
			if _, err := parseLoad([]byte(data), time.Now(), time.Now(), time.Hour); err == nil {
				t.Fatal("load accepted invalid JSON")
			}
		})
	}
}

func TestApplicationRejectsDuplicatesInOtherwiseValidObservation(t *testing.T) {
	base := `{"deployment":{"version":"v2","prefix":"deploy/v2/","generation":2,"swapping":0,"cells":{"a":2}}`
	for _, suffix := range []string{
		`,"unobserved":{"a":1,"a":2}}`,
		`,"shutdown":{"runtime_generation":"a","\u0072untime_generation":"b"}}`,
		`,"deployment":{"version":"v2","prefix":"deploy/v2/","generation":2,"swapping":0,"cells":{}}}`,
	} {
		if _, err := decodeApplication([]byte(base+suffix), time.Now()); err == nil {
			t.Fatal("duplicate fields accepted in application observation")
		}
	}
	data := `{"deployment":{"version":"v2","prefix":"deploy/v2/","generation":2,"swapping":0,"cells":{"a":1,"\u0061":2}}}`
	if _, err := decodeApplication([]byte(data), time.Now()); err == nil {
		t.Fatal("duplicate census entries accepted in application observation")
	}
}

func TestValidObjectBoundaryCompatibility(t *testing.T) {
	for _, data := range []string{
		`{}`,
		" \n\t {} \n",
		`{"array":[{"key":"value"},1,true,false,null]}`,
		`{"a":` + strings.Repeat("[", 63) + `0` + strings.Repeat("]", 63) + `}`,
		// As before, an empty container may begin at depth 64; a value inside
		// it would exceed the budget, checked by the excessive-depth case.
		`{"a":` + strings.Repeat("[", 64) + strings.Repeat("]", 64) + `}`,
		`{"invalid-utf8":"` + string([]byte{0xff}) + `"}`,
		`{"surrogate":"\ud800"}`,
	} {
		if err := validObject([]byte(data)); err != nil {
			t.Errorf("compatible JSON rejected: %q: %v", data, err)
		}
	}
}

func TestApplicationCensusNumericBoundaries(t *testing.T) {
	for _, value := range []string{"null", "-1", "18446744073709551616", "1.0", "1e0", "true", "{}", "[]", `"1"`} {
		data := `{"deployment":{"version":"v2","prefix":"deploy/v2/","generation":2,"swapping":0,"cells":{"cell":` + value + `}}}`
		if _, err := decodeApplication([]byte(data), time.Now()); err == nil {
			t.Errorf("accepted non-integer cell generation %s", value)
		}
	}
}
