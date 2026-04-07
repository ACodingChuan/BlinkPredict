package marketconfirm

import (
	"errors"
	"testing"
)

func TestRetryableVerifyErrorTag(t *testing.T) {
	err := markVerifyRetryable(errors.New("metadata unavailable"))
	if !isRetryableVerifyError(err) {
		t.Fatal("expected retryable verify error to be detected")
	}
	if isRetryableVerifyError(errors.New("plain")) {
		t.Fatal("did not expect plain error to be marked retryable")
	}
}
