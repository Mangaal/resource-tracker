package env

import (
	"os"
	"testing"
)

func TestGetStringVal(t *testing.T) {
	t.Run("returns environment variable value when set", func(t *testing.T) {
		envVar := "TEST_ENV_VAR"
		expectedValue := "test_value"
		os.Setenv(envVar, expectedValue)
		defer os.Unsetenv(envVar)

		result := GetStringVal(envVar, "default_value")
		if result != expectedValue {
			t.Errorf("expected %s, got %s", expectedValue, result)
		}
	})

	t.Run("returns default value when environment variable is not set", func(t *testing.T) {
		envVar := "UNSET_ENV_VAR"
		defaultValue := "default_value"

		result := GetStringVal(envVar, defaultValue)
		if result != defaultValue {
			t.Errorf("expected %s, got %s", defaultValue, result)
		}
	})

	t.Run("returns default value when environment variable is empty", func(t *testing.T) {
		envVar := "EMPTY_ENV_VAR"
		defaultValue := "default_value"
		os.Setenv(envVar, "")
		defer os.Unsetenv(envVar)

		result := GetStringVal(envVar, defaultValue)
		if result != defaultValue {
			t.Errorf("expected %s, got %s", defaultValue, result)
		}
	})
}
