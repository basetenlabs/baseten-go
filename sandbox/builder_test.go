package sandbox

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/internal/require"
)

var injectedSandboxAPI = []string{
	"COPY --from=ghcr.io/blaxel-ai/sandbox:latest /sandbox-api /usr/local/bin/sandbox-api",
	`ENTRYPOINT ["/usr/local/bin/sandbox-api"]`,
}

func baseBuilder() *ImageBuilder {
	return NewImageBuilder("debian:bookworm-slim")
}

// expectedDockerfile is the base builder's Dockerfile with lines, then the
// injected sandbox API lines.
func expectedDockerfile(lines ...string) string {
	all := append([]string{"FROM debian:bookworm-slim"}, lines...)
	return strings.Join(append(all, injectedSandboxAPI...), "\n") + "\n"
}

func requireDockerfile(t *testing.T, b *ImageBuilder) string {
	t.Helper()
	dockerfile, err := b.Dockerfile()
	require.NoError(t, err)
	return dockerfile
}

func requireBuilderError(t *testing.T, b *ImageBuilder, contains string) {
	t.Helper()
	_, err := b.Dockerfile()
	require.Error(t, err)
	require.Contains(t, err.Error(), contains)
}

func TestImageBuilder(t *testing.T) {
	t.Run("CoreInstructionsThenSandboxAPI", func(t *testing.T) {
		b := baseBuilder().
			Workdir("/app").
			RunCommands("echo one", "echo two").
			Env(map[string]string{"B": "two words", "A": "1"}).
			Copy("src dir", "/app/src").
			Expose(8080, 9090).
			User("app").
			Label(map[string]string{"team": "infra"}).
			Arg("VERSION", "").
			Arg("CHANNEL", "stable")
		require.Equal(t, "debian:bookworm-slim", b.BaseImage())
		require.Equal(t, expectedDockerfile(
			"WORKDIR /app",
			"RUN echo one",
			"RUN echo two",
			// In name order, whatever the map's order.
			`ENV A="1"`,
			`ENV B="two words"`,
			`COPY ["src dir","/app/src"]`,
			"EXPOSE 8080",
			"EXPOSE 9090",
			"USER app",
			`LABEL team="infra"`,
			"ARG VERSION",
			`ARG CHANNEL="stable"`,
		), requireDockerfile(t, b))
	})

	t.Run("EscapesQuotesAndBackslashesLeavingDollar", func(t *testing.T) {
		b := baseBuilder().
			Env(map[string]string{"PATH": "/opt/bin:$PATH", "QUOTE": `say "hi" \ bye`}).
			Label(map[string]string{"note": `a "b"`}).
			Arg("X", `c"d`)
		require.Equal(t, expectedDockerfile(
			`ENV PATH="/opt/bin:$PATH"`,
			`ENV QUOTE="say \"hi\" \\ bye"`,
			`LABEL note="a \"b\""`,
			`ARG X="c\"d"`,
		), requireDockerfile(t, b))
	})

	t.Run("NewlineFailsAnywhereButRawLines", func(t *testing.T) {
		requireBuilderError(t, NewImageBuilder("a\nb"), "BaseImage must not contain a newline")
		requireBuilderError(t, baseBuilder().Workdir("/a\n/b"), "Workdir path must not contain a newline")
		requireBuilderError(t, baseBuilder().RunCommands("a\nb"), "RunCommands command must not contain a newline")
		requireBuilderError(t, baseBuilder().Env(map[string]string{"A": "1\r\n2"}), "Env value must not contain a newline")
		requireBuilderError(t, baseBuilder().Copy("a\n", "/b"), "Copy source must not contain a newline")
		requireBuilderError(t, baseBuilder().Entrypoint("a\nb"), "Entrypoint argument must not contain a newline")
		requireBuilderError(t, baseBuilder().PipInstall("a\nb"), "pip install argument must not contain a newline")
	})

	t.Run("NamesThatCannotBeUnquotedFail", func(t *testing.T) {
		requireBuilderError(t, baseBuilder().Env(map[string]string{"A B": "1"}), `Env name "A B"`)
		requireBuilderError(t, baseBuilder().Label(map[string]string{"a=b": "1"}), `Label key "a=b"`)
		requireBuilderError(t, baseBuilder().Arg("", ""), `Arg name ""`)
	})

	t.Run("PortsOutOfRangeFail", func(t *testing.T) {
		requireBuilderError(t, baseBuilder().Expose(0), "Expose port 0 is not between 1 and 65535")
		requireBuilderError(t, baseBuilder().Expose(65536), "Expose port 65536")
	})

	t.Run("RecordsEveryErrorAndKeepsGoing", func(t *testing.T) {
		b := baseBuilder().Workdir("/a\n").RunCommands("echo ok").Expose(0).User("u\n")
		_, err := b.Dockerfile()
		require.Error(t, err)
		for _, message := range []string{"Workdir path", "Expose port 0", "User must not"} {
			require.Contains(t, err.Error(), message)
		}
	})

	t.Run("BaseImageRequired", func(t *testing.T) {
		requireBuilderError(t, NewImageBuilder(""), "BaseImage is required")
		// A builder made without the constructor.
		requireBuilderError(t, &ImageBuilder{}, "create it with NewImageBuilder")
	})

	t.Run("RawLinesVerbatim", func(t *testing.T) {
		heredoc := "RUN <<EOF\necho one\necho two\nEOF"
		require.Equal(t, expectedDockerfile("# a comment", heredoc),
			requireDockerfile(t, baseBuilder().DockerfileLines("# a comment", heredoc)))
	})

	t.Run("EntrypointInExecFormReplacesDefault", func(t *testing.T) {
		require.Equal(t, strings.Join([]string{
			"FROM debian:bookworm-slim",
			`ENTRYPOINT ["/bin/sh","-c","echo \"hi\" <&>"]`,
			injectedSandboxAPI[0],
		}, "\n")+"\n", requireDockerfile(t, baseBuilder().Entrypoint("/bin/sh", "-c", `echo "hi" <&>`)))
		require.Equal(t, expectedDockerfile(), requireDockerfile(t, baseBuilder().Entrypoint()))
	})

	t.Run("EntrypointInRawLinesCountsButNotInComment", func(t *testing.T) {
		require.False(t, strings.Contains(requireDockerfile(t, baseBuilder().DockerfileLines(`entrypoint ["/run"]`)), injectedSandboxAPI[1]),
			"default entrypoint injected")
		require.Contains(t, requireDockerfile(t, baseBuilder().DockerfileLines(`# ENTRYPOINT ["/run"]`)), injectedSandboxAPI[1])
	})

	t.Run("SandboxAPICopySkippedWhenBroughtIn", func(t *testing.T) {
		require.False(t, strings.Contains(requireDockerfile(t, baseBuilder().RunCommands("curl -o /usr/local/bin/sandbox-api https://x")), injectedSandboxAPI[0]),
			"sandbox API copy injected")
		require.False(t, strings.Contains(requireDockerfile(t, NewImageBuilder("ghcr.io/blaxel-ai/sandbox:v1")), injectedSandboxAPI[0]),
			"sandbox API copy injected")
	})

	t.Run("SandboxAPIFromGivenImage", func(t *testing.T) {
		b := NewImageBuilderWithOptions(ImageBuilderOptions{BaseImage: "debian:bookworm-slim", SandboxAPIImage: "registry.example/sandbox:v2"})
		require.Contains(t, requireDockerfile(t, b), "COPY --from=registry.example/sandbox:v2 /sandbox-api /usr/local/bin/sandbox-api")
	})

	t.Run("NeverChanges", func(t *testing.T) {
		shared := baseBuilder().PipInstall("numpy")
		withTorch := shared.PipInstall("torch")
		withJax := shared.PipInstall("jax")
		failed := shared.Workdir("\n")
		withFile := shared.AddFile("/a.txt", []byte("a"))
		require.Equal(t, expectedDockerfile("RUN pip install numpy"), requireDockerfile(t, shared))
		require.Equal(t, expectedDockerfile("RUN pip install numpy", "RUN pip install torch"), requireDockerfile(t, withTorch))
		require.Equal(t, expectedDockerfile("RUN pip install numpy", "RUN pip install jax"), requireDockerfile(t, withJax))
		requireBuilderError(t, failed, "Workdir path")
		require.Len(t, shared.context, 0)
		require.Len(t, withFile.context, 1)
	})

	t.Run("PackageManagerCommandsQuotingWhenNeeded", func(t *testing.T) {
		b := baseBuilder().
			PipInstall("--pre", "numpy>=2", "it's").
			AptInstall("git", "curl").
			ApkAdd("git").
			NpmInstall("-g", "typescript@5").
			NpmInstall().
			GemInstall("rails").
			CargoInstall("--locked", "ripgrep").
			GoInstall("golang.org/x/tools/gopls@latest", "example.com/cmd@v1").
			ComposerInstall("laravel/framework").
			UvInstall("ruff").
			PipxInstall("black", "httpie")
		require.Equal(t, expectedDockerfile(
			`RUN pip install --pre 'numpy>=2' 'it'\''s'`,
			"RUN apt-get update && apt-get install -y --no-install-recommends git curl && rm -rf /var/lib/apt/lists/*",
			"RUN apk add --no-cache git",
			"RUN npm install -g typescript@5",
			"RUN npm install",
			"RUN gem install --no-document rails",
			"RUN cargo install --locked ripgrep",
			"RUN go install golang.org/x/tools/gopls@latest && go install example.com/cmd@v1",
			"RUN composer require laravel/framework",
			"RUN uv pip install --system ruff",
			"RUN pipx install black && pipx install httpie",
		), requireDockerfile(t, b))
	})

	t.Run("NothingWithoutArgumentsExceptNpm", func(t *testing.T) {
		b := baseBuilder().
			PipInstall().
			AptInstall().
			ApkAdd().
			GemInstall().
			CargoInstall().
			GoInstall().
			ComposerInstall().
			UvInstall().
			PipxInstall().
			RunCommands().
			Env(nil).
			Expose()
		require.Equal(t, expectedDockerfile(), requireDockerfile(t, b))
	})

	t.Run("ContextNamesByBaseNameSuffixingRepeatsAndReserved", func(t *testing.T) {
		b := baseBuilder().
			AddFile("/etc/a/config.json", []byte("1")).
			AddFile("/etc/b/config.json", []byte("2")).
			AddFile("/etc/c/config.json", []byte("3")).
			AddFile("/srv/Makefile", []byte("4")).
			AddFile("/opt/Makefile", []byte("5")).
			AddFile("/x/Dockerfile", []byte("6")).
			AddFile("/x/.dockerignore", []byte("7"))
		require.Equal(t, expectedDockerfile(
			`COPY ["config.json","/etc/a/config.json"]`,
			`COPY ["config (1).json","/etc/b/config.json"]`,
			`COPY ["config (2).json","/etc/c/config.json"]`,
			`COPY ["Makefile","/srv/Makefile"]`,
			`COPY ["Makefile (1)","/opt/Makefile"]`,
			`COPY ["Dockerfile (1)","/x/Dockerfile"]`,
			`COPY [".dockerignore (1)","/x/.dockerignore"]`,
		), requireDockerfile(t, b))
	})

	t.Run("ExplicitContextNameExactOrFails", func(t *testing.T) {
		b := baseBuilder().AddFileWithOptions(ImageBuilderAddFileOptions{Destination: "/app/run.sh", Content: []byte("x"), ContextName: "start.sh"})
		require.Contains(t, requireDockerfile(t, b), `COPY ["start.sh","/app/run.sh"]`)
		requireBuilderError(t, b.AddFileWithOptions(ImageBuilderAddFileOptions{Destination: "/b", ContextName: "start.sh"}),
			`context name "start.sh" is reserved or already used`)
		requireBuilderError(t, baseBuilder().AddFileWithOptions(ImageBuilderAddFileOptions{Destination: "/b", ContextName: "Dockerfile"}),
			`context name "Dockerfile" is reserved or already used`)
		requireBuilderError(t, baseBuilder().AddFileWithOptions(ImageBuilderAddFileOptions{Destination: "/b", ContextName: "a/b"}),
			`context name "a/b" must be one path segment`)
	})

	t.Run("DestinationEndingInSlashNeedsContextName", func(t *testing.T) {
		requireBuilderError(t, baseBuilder().AddFile("/app/", []byte("x")), "AddFile Destination /app/ has no file name; set ContextName")
		b := baseBuilder().AddFileWithOptions(ImageBuilderAddFileOptions{Destination: "/app/", Content: []byte("x"), ContextName: "x.txt"})
		require.Contains(t, requireDockerfile(t, b), `COPY ["x.txt","/app/"]`)
	})

	t.Run("RequiredFields", func(t *testing.T) {
		requireBuilderError(t, baseBuilder().AddFile("", nil), "AddFile Destination is required")
		requireBuilderError(t, baseBuilder().AddLocalFile("", "/a"), "AddLocalFile Source is required")
		requireBuilderError(t, baseBuilder().AddLocalDir("/a", ""), "AddLocalDir Destination is required")
	})
}

func TestImageBuilderZipEntries(t *testing.T) {
	t.Run("DockerfileAndEveryEntryReadingLocalAtPush", func(t *testing.T) {
		dir := t.TempDir()
		tool := filepath.Join(dir, "tool.sh")
		require.NoError(t, os.WriteFile(tool, []byte("before"), 0o755))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "conf", "nested"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "conf", "nested", "a.txt"), []byte("a"), 0o644))
		content := []byte{0, 255}
		b := baseBuilder().
			AddFile("/etc/text.txt", []byte("text")).
			AddFile("/etc/bytes.bin", content).
			AddFileWithOptions(ImageBuilderAddFileOptions{Destination: "/usr/local/bin/run", Content: []byte("#!/bin/sh"), Mode: 0o755}).
			AddLocalFile(tool, "/usr/local/bin/tool").
			AddLocalDir(filepath.Join(dir, "conf"), "/etc/conf")
		// Read when zipped, not when added, while AddFile content is copied
		// when added.
		require.NoError(t, os.WriteFile(tool, []byte("after"), 0o755))
		content[0] = 9

		entries, err := b.zipEntries(t.Context())
		require.NoError(t, err)
		var paths []string
		byPath := map[string]zipEntry{}
		for _, entry := range entries {
			paths = append(paths, entry.path)
			byPath[entry.path] = entry
		}
		require.Equal(t, "Dockerfile,text.txt,bytes.bin,run,tool.sh,conf,conf/nested,conf/nested/a.txt", strings.Join(paths, ","))
		require.Equal(t, requireDockerfile(t, b), string(byPath["Dockerfile"].data))
		require.Equal(t, 0o644, int(byPath["Dockerfile"].mode))
		require.Equal(t, "text", string(byPath["text.txt"].data))
		require.Equal(t, 0o644, int(byPath["text.txt"].mode))
		require.Equal(t, "\x00\xff", string(byPath["bytes.bin"].data))
		require.Equal(t, 0o755, int(byPath["run"].mode))
		require.Equal(t, "after", zipEntryContent(t, byPath["tool.sh"]))
		require.True(t, byPath["conf"].dir, "conf is not a directory")
		require.Equal(t, "a", zipEntryContent(t, byPath["conf/nested/a.txt"]))
		// Windows has no exec bits to keep.
		if runtime.GOOS != "windows" {
			require.Equal(t, 0o755, int(byPath["tool.sh"].mode))
		}
		require.Contains(t, requireDockerfile(t, b), `COPY ["tool.sh","/usr/local/bin/tool"]`)
		require.Contains(t, requireDockerfile(t, b), `COPY ["conf","/etc/conf"]`)
	})

	t.Run("LocalGoneFailsAtPush", func(t *testing.T) {
		dir := t.TempDir()
		gone := filepath.Join(dir, "gone.txt")
		require.NoError(t, os.WriteFile(gone, []byte("x"), 0o644))
		b := baseBuilder().AddLocalFile(gone, "/gone.txt")
		require.NoError(t, os.Remove(gone))
		_, err := b.zipEntries(t.Context())
		require.Error(t, err)
		_, err = baseBuilder().AddLocalDir(filepath.Join(dir, "missing"), "/m").zipEntries(t.Context())
		require.Error(t, err)
		require.NoError(t, os.WriteFile(gone, []byte("x"), 0o644))
		_, err = baseBuilder().AddLocalDir(gone, "/m").zipEntries(t.Context())
		require.Error(t, err)
		require.Contains(t, err.Error(), "is not a directory")
	})
}

func TestImagePushBuilder(t *testing.T) {
	t.Run("PushesZipOfDockerfileAndFiles", func(t *testing.T) {
		cp := newControlPlane(t)
		store := newStorage(t, http.StatusOK)
		cp.respond("POST", "/v1/sandboxes/images", 202, `{"name":"img","status":"UPLOADING","upload_url":"`+store.URL+`/put"}`)
		b := baseBuilder().AddFile("/hello.txt", []byte("hello"))
		_, err := cp.client(t, ClientOptions{}).Images().Push(t.Context(), ImagePushOptions{Name: "img", SourceBuilder: b, NoWait: true})
		require.NoError(t, err)
		contents, _ := readZip(t, store.body)
		require.Equal(t, 2, len(contents))
		require.MapEqual(t, contents, "Dockerfile", requireDockerfile(t, b))
		require.MapEqual(t, contents, "hello.txt", "hello")
	})

	t.Run("BuilderErrorsFailBeforeSending", func(t *testing.T) {
		cp := newControlPlane(t)
		_, err := cp.client(t, ClientOptions{}).Images().Push(t.Context(), ImagePushOptions{
			Name:          "img",
			SourceBuilder: baseBuilder().Workdir("\n"),
			PollInterval:  time.Millisecond,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "Workdir path")
		require.Len(t, cp.requests, 0)
	})

	t.Run("BuilderAndAnotherSourceFails", func(t *testing.T) {
		cp := newControlPlane(t)
		_, err := cp.client(t, ClientOptions{}).Images().Push(t.Context(), ImagePushOptions{
			Name:          "img",
			SourceBuilder: baseBuilder(),
			SourceFiles:   dockerfileOnly,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "got SourceBuilder, SourceFiles")
	})
}
