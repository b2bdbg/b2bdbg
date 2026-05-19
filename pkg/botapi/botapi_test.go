package botapi_test

import (
	"testing"

	"github.com/b2bdbg/b2bdbg/pkg/botapi"
)

func TestParseMethod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		wantMethod string
		wantToken  string
	}{
		{
			name:       "standard getMe path",
			path:       "/bot1234567890:ABCDEF/getMe",
			wantMethod: "getMe",
			wantToken:  "1234567890:ABCDEF",
		},
		{
			name:       "standard sendMessage path",
			path:       "/bot999:TOKEN/sendMessage",
			wantMethod: "sendMessage",
			wantToken:  "999:TOKEN",
		},
		{
			name:       "getUpdates path",
			path:       "/bot1:T/getUpdates",
			wantMethod: "getUpdates",
			wantToken:  "1:T",
		},
		{
			name:       "extra trailing segments ignored",
			path:       "/bot1:T/sendMessage/extra",
			wantMethod: "sendMessage",
			wantToken:  "1:T",
		},
		{
			name:       "no method segment returns empty",
			path:       "/bot1234:TOKEN",
			wantMethod: "",
			wantToken:  "",
		},
		{
			name:       "no bot prefix returns empty",
			path:       "/api/getMe",
			wantMethod: "",
			wantToken:  "",
		},
		{
			name:       "empty path returns empty",
			path:       "",
			wantMethod: "",
			wantToken:  "",
		},
		{
			name:       "root slash only",
			path:       "/",
			wantMethod: "",
			wantToken:  "",
		},
		{
			name:       "real-looking token",
			path:       "/bot110201543:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/sendMessage",
			wantMethod: "sendMessage",
			wantToken:  "110201543:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotMethod, gotToken := botapi.ParseMethod(tc.path)

			if gotMethod != tc.wantMethod {
				t.Errorf("ParseMethod(%q) method = %q, want %q", tc.path, gotMethod, tc.wantMethod)
			}
			if gotToken != tc.wantToken {
				t.Errorf("ParseMethod(%q) token = %q, want %q", tc.path, gotToken, tc.wantToken)
			}
		})
	}
}
