package output_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jeon-jihyeon/rindcut/internal/output"
)

func TestJSON(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer

	require.NoError(t, output.JSON(&b, map[string]string{"url": "https://x?a=1&b=<2>"}))

	assert.Equal(t, "{\n  \"url\": \"https://x?a=1&b=<2>\"\n}\n", b.String())
}

func TestTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		table output.Table
		want  string
	}{
		{
			name:  "header and rows aligned",
			table: output.Table{Header: []string{"JOB", "VERDICT"}, Rows: [][]string{{"price-watch", "pass"}, {"a", "missed"}}},
			want:  "JOB          VERDICT\nprice-watch  pass\na            missed\n",
		},
		{
			name:  "rows only",
			table: output.Table{Rows: [][]string{{"a", "b"}}},
			want:  "a  b\n",
		},
		{name: "empty", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer

			require.NoError(t, tt.table.Write(&b))

			assert.Equal(t, tt.want, b.String())
		})
	}
}
