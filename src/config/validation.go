package config

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"
)

// Registration finishes before any caller can use the shared validator.
var fields = func() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	v.RegisterTagNameFunc(func(field reflect.StructField) string { return field.Tag.Get("koanf") })
	if err := v.RegisterValidation("finite", func(field validator.FieldLevel) bool {
		value := field.Field().Float()
		return !math.IsNaN(value) && !math.IsInf(value, 0)
	}); err != nil {
		panic(err)
	}
	return v
}()

func validateFields(at string, value any) error {
	err := fields.Struct(value)
	if err == nil {
		return nil
	}
	var failures validator.ValidationErrors
	if !errors.As(err, &failures) {
		return fmt.Errorf("%s: %w", at, err)
	}
	failure := failures[0]
	_, path, _ := strings.Cut(failure.Namespace(), ".")
	if at != "" {
		path = at + "." + path
	}
	// Do not include field values: configuration can hold credentials.
	return fmt.Errorf("%s: fails %s validation (%s)", path, failure.Tag(), failure.Param())
}
