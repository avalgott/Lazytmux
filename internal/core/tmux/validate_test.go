package tmux

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateSessionName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		wantErr string // empty = no error
	}{
		{name: "plain", in: "devbox"},
		{name: "spaces allowed", in: "my session"},
		{name: "empty", in: "", wantErr: "session name is required"},
		{name: "colon", in: "a:b", wantErr: "invalid character"},
		{name: "dot", in: "a.b", wantErr: "invalid character"},
		{name: "semicolon", in: "a;rm", wantErr: "unsafe character"},
		{name: "ampersand", in: "a&b", wantErr: "unsafe character"},
		{name: "dollar", in: "a$b", wantErr: "unsafe character"},
		{name: "paren", in: "a(b", wantErr: "unsafe character"},
		{name: "newline", in: "a\nb", wantErr: "unsafe character"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSessionName(tt.in)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			if assert.Error(t, err) {
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}
