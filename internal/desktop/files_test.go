package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseHostCopyPaths(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []HostCopyPath
		wantErr bool
	}{
		{
			name:  "empty",
			input: "",
			want:  nil,
		},
		{
			name:  "whitespace only",
			input: "   \n  \t ",
			want:  nil,
		},
		{
			name:  "json array of objects",
			input: `[{"src":"C:\\data\\file.txt","dest":"Desktop/file.txt"},{"src":"/var/log","dest":"logs"}]`,
			want: []HostCopyPath{
				{Src: `C:\data\file.txt`, Dest: "Desktop/file.txt"},
				{Src: "/var/log", Dest: "logs"},
			},
		},
		{
			name:  "json array of string specs",
			input: `["C:\\data\\file.txt -> myfile.txt", "/home/user/doc.pdf => Desktop/doc.pdf", "/opt/data"]`,
			want: []HostCopyPath{
				{Src: `C:\data\file.txt`, Dest: "myfile.txt"},
				{Src: "/home/user/doc.pdf", Dest: "Desktop/doc.pdf"},
				{Src: "/opt/data", Dest: ""},
			},
		},
		{
			name: "multiline with comments and arrows",
			input: "# Comments to ignore\n" +
				"C:\\projects\\app -> Desktop/app\n" +
				"/home/user/report.pdf => report.pdf\n" +
				"\n" +
				"/var/data\n",
			want: []HostCopyPath{
				{Src: `C:\projects\app`, Dest: "Desktop/app"},
				{Src: "/home/user/report.pdf", Dest: "report.pdf"},
				{Src: "/var/data", Dest: ""},
			},
		},
		{
			name:  "semicolon separated with colons and Windows drive letters",
			input: `C:\my\file.txt:sub/file.txt; /data/set -> data`,
			want: []HostCopyPath{
				{Src: `C:\my\file.txt`, Dest: "sub/file.txt"},
				{Src: "/data/set", Dest: "data"},
			},
		},
		{
			name:  "quoted paths",
			input: `"C:\path with spaces\file.txt" -> "Desktop/target name.txt"`,
			want: []HostCopyPath{
				{Src: `C:\path with spaces\file.txt`, Dest: "Desktop/target name.txt"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseHostCopyPaths(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseHostCopyPaths() error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ParseHostCopyPaths() got %d items, want %d: %+v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i].Src != tt.want[i].Src || got[i].Dest != tt.want[i].Dest {
					t.Errorf("item[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestResolveVMDestPath(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		dest    string
		isDir   bool
		want    string
		wantErr bool
	}{
		{
			name:  "empty dest defaults under /home/headless",
			src:   "C:/Users/name/document.pdf",
			dest:  "",
			isDir: false,
			want:  "/home/headless/document.pdf",
		},
		{
			name:  "relative dest prepends /home/headless",
			src:   "/tmp/myfile.txt",
			dest:  "Desktop/myfile.txt",
			isDir: false,
			want:  "/home/headless/Desktop/myfile.txt",
		},
		{
			name:  "explicit /home/headless/ path preserved",
			src:   "mydata.csv",
			dest:  "/home/headless/data/mydata.csv",
			isDir: false,
			want:  "/home/headless/data/mydata.csv",
		},
		{
			name:  "trailing slash appends basename",
			src:   "C:/code/project",
			dest:  "Desktop/",
			isDir: true,
			want:  "/home/headless/Desktop/project",
		},
		{
			name:  "directory destination /home/headless/Desktop",
			src:   "/home/user/images",
			dest:  "/home/headless/Desktop",
			isDir: true,
			want:  "/home/headless/Desktop/images",
		},
		{
			name:    "traversal outside /home/headless fails",
			src:     "test.txt",
			dest:    "/etc/shadow",
			isDir:   false,
			wantErr: true,
		},
		{
			name:    "relative traversal outside fails",
			src:     "test.txt",
			dest:    "../../etc/passwd",
			isDir:   false,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveVMDestPath(tt.src, tt.dest, tt.isDir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ResolveVMDestPath() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ResolveVMDestPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestComputeDefaultVMDest(t *testing.T) {
	userHome, _ := os.UserHomeDir()
	if userHome != "" {
		hostDoc := filepath.Join(userHome, "Documents", "report.pdf")
		got := ComputeDefaultVMDest(hostDoc)
		want := "/home/headless/Documents/report.pdf"
		if got != want {
			t.Errorf("ComputeDefaultVMDest(%q) = %q, want %q", hostDoc, got, want)
		}
	}

	// Test anchor folder detection for paths outside home
	got := ComputeDefaultVMDest("D:/data/projects/my-app")
	want := "/home/headless/projects/my-app"
	if got != want {
		t.Errorf("ComputeDefaultVMDest anchor got %q, want %q", got, want)
	}

	gotBase := ComputeDefaultVMDest("D:/random/unknown/file.txt")
	wantBase := "/home/headless/file.txt"
	if gotBase != wantBase {
		t.Errorf("ComputeDefaultVMDest fallback got %q, want %q", gotBase, wantBase)
	}
}
