package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestP1F16Probe(t *testing.T) {
	ext := gbkBytes(t, "丂")
	big := append(bytes.Repeat(ext, 32767), 'x')
	big = append(big, ext...)
	cases := []struct {
		name string
		body []byte
		want TextEncoding
	}{
		{"short-invalid", []byte{'a', 'b', 'c', 255}, EncUnknown},
		{"split-extension", big, EncGBK},
		{"full-window-invalid", append(bytes.Repeat([]byte{'a'}, 65535), 255), EncUnknown},
		{"interior-invalid", append(append([]byte{255}, bytes.Repeat([]byte{'a'}, 65534)...), ext...), EncUnknown},
		{"long-invalid-tail", append(append(bytes.Repeat([]byte{'a'}, 65535), 255), 'a', 'a', 'a'), EncUnknown},
		{"utf8", []byte("正常文件"), EncUTF8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "input.txt")
			if err := os.WriteFile(p, c.body, 0600); err != nil {
				t.Fatal(err)
			}
			got := probeTextFile(p)
			t.Logf("got=%s want=%s", got, c.want)
			if got != c.want {
				t.Fail()
			}
		})
	}
}
