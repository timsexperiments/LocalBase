package childenv

import (
	"slices"
	"testing"
)

func TestCredentialsStayBehind(t *testing.T) {
	for _, entry := range []string{
		"LOCALBASE_API_KEY=x", "LOCALBASE_SERVICE_TOKEN=x", "LOCALBASE_SERVICE_ID=x", "localbase_api_key=x",
		"HF_TOKEN=x", "HUGGING_FACE_HUB_TOKEN=x", "OTEL_EXPORTER_OTLP_HEADERS=x", "OTEL_EXPORTER_OTLP_TRACES_HEADERS=x",
		"GITHUB_TOKEN=x", "AWS_SECRET_ACCESS_KEY=x", "AWS_ACCESS_KEY_ID=x", "OPENAI_API_KEY=x", "ANTHROPIC_APIKEY=x",
		"CLIENT_SECRET=x", "APP_SECRETS=x", "DB_PASSWORD=x", "SMB_PASSWD=x", "MYSQL_PWD=x", "AWS_SESSION_TOKEN=x", "GH_TOKEN=x", "AZURE_CREDENTIAL=x", "GOOGLE_APPLICATION_CREDENTIALS=x", "TLS_PRIVATE_KEY=x", "SSH_AUTH_SOCK=x",
	} {
		if got := Clean([]string{entry}); len(got) != 0 {
			t.Errorf("%s got through", entry)
		}
	}
}

func TestWhatABackendNeedsStays(t *testing.T) {
	keep := []string{
		"PATH=/usr/bin", "HOME=/home/a", "USER=a", "LANG=C", "LC_ALL=C", "TMPDIR=/tmp", "TZ=UTC", "SHELL=/bin/sh",
		"LD_LIBRARY_PATH=/x", "DYLD_LIBRARY_PATH=/x", "DYLD_FALLBACK_LIBRARY_PATH=/x", "CUDA_VISIBLE_DEVICES=0", "CUDA_DEVICE_ORDER=PCI_BUS_ID",
		"HF_HOME=/x", "TOKENIZERS_PARALLELISM=false", "HF_TOKENIZERS_CACHE=/x", "LLAMA_MAX_TOKENS=8", "SECRETARY=x", "PWD=/x", "OLDPWD=/x", "OMP_NUM_THREADS=4", "OTEL_SERVICE_NAME=x", "CRISPASR_MISAKI_DICT_PATH=/x", "FAKE_CANCEL_LOG=/x", "EMPTY=", "NOEQUALS",
	}
	if got := Clean(keep); !slices.Equal(got, keep) {
		t.Errorf("got %v", got)
	}
}

// What comes in stays in its order, since a later entry for a name wins.
func TestOrderIsKept(t *testing.T) {
	in := []string{"A=1", "HF_TOKEN=x", "A=2", "B=3"}
	if got := Clean(in); !slices.Equal(got, []string{"A=1", "A=2", "B=3"}) {
		t.Errorf("got %v", got)
	}
	if len(in) != 4 {
		t.Error("the input changed")
	}
}
