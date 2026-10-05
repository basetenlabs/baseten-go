package sandbox

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

const (
	defaultSandboxAPIImage = "ghcr.io/blaxel-ai/sandbox:latest"
	sandboxAPIPath         = "/usr/local/bin/sandbox-api"
	defaultAddFileMode     = fs.FileMode(0o644)
)

var (
	// Characters a package manager argument may have and still be passed to
	// the shell unquoted.
	shellSafe = regexp.MustCompile(`^[A-Za-z0-9._\-+=:/@~^*]+$`)

	entrypointLine = regexp.MustCompile(`(?i)^\s*ENTRYPOINT\b`)
)

// ImageBuilder describes an image as a Dockerfile and the files it copies in,
// to push with [ImagePushOptions.SourceBuilder]. Start one with
// [NewImageBuilder].
//
// A builder never changes: each method returns a new builder with the
// instruction appended, so one builder can safely be the base of several.
//
// Invalid input, such as a newline in a value that must fit on one line, does
// not stop the chain. The method leaves its instruction out and records the
// error, and [ImageBuilder.Dockerfile] and the push return every recorded
// error.
//
// Package helpers such as [ImageBuilder.PipInstall] pass each argument to the
// shell unquoted if it has only letters, digits, and ._-+=:/@~^*, and
// single-quote it otherwise. So * and ~ still expand, as in
// PipInstall("dist/*.whl"), while characters such as >, ;, $, and spaces stay
// literal. Use [ImageBuilder.RunCommands] for full control.
//
// The Dockerfile it pushes ends by copying in the sandbox API binary and
// making it the entrypoint, unless the builder already did either, since a
// sandbox needs it running to be reached. [ImageBuilder.Dockerfile] shows the
// Dockerfile exactly as pushed.
type ImageBuilder struct {
	baseImage       string
	sandboxAPIImage string
	instructions    []string
	context         []imageContextEntry
	errs            []error
}

// imageContextEntry is a file or directory added to the build context.
type imageContextEntry struct {
	name string
	// sourcePath is set for a local file or directory, read at push.
	// Otherwise the entry is content, with its mode.
	sourcePath string
	dir        bool
	content    []byte
	mode       fs.FileMode
}

// ImageBuilderOptions are the options for [NewImageBuilderWithOptions].
type ImageBuilderOptions struct {
	// BaseImage is the registry image to start from, as FROM <BaseImage>.
	// Required.
	BaseImage string

	// SandboxAPIImage is the image to copy the sandbox API binary from, when
	// the Dockerfile does not bring it in itself. Empty uses
	// ghcr.io/blaxel-ai/sandbox:latest.
	SandboxAPIImage string
}

// NewImageBuilder starts a builder from a registry image, as FROM <baseImage>.
// See [NewImageBuilderWithOptions] for more options.
func NewImageBuilder(baseImage string) *ImageBuilder {
	return NewImageBuilderWithOptions(ImageBuilderOptions{BaseImage: baseImage})
}

// NewImageBuilderWithOptions starts a builder from a registry image, as FROM
// <BaseImage>.
func NewImageBuilderWithOptions(opts ImageBuilderOptions) *ImageBuilder {
	b := &ImageBuilder{
		baseImage:       opts.BaseImage,
		sandboxAPIImage: cmp.Or(opts.SandboxAPIImage, defaultSandboxAPIImage),
	}
	if opts.BaseImage == "" {
		return b.with(nil, nil, errors.New("BaseImage is required"))
	}
	return b.with(nil, nil, singleLine("BaseImage", opts.BaseImage), singleLine("SandboxAPIImage", opts.SandboxAPIImage))
}

// BaseImage is the image this builder starts from.
func (b *ImageBuilder) BaseImage() string {
	return b.baseImage
}

// Dockerfile is the Dockerfile exactly as it is pushed, or every error the
// builder recorded, joined.
func (b *ImageBuilder) Dockerfile() (string, error) {
	if err := errors.Join(b.errs...); err != nil {
		return "", err
	}
	// Only a builder made without NewImageBuilder has no base image and no
	// error saying so.
	if b.baseImage == "" {
		return "", errors.New("ImageBuilder has no base image; create it with NewImageBuilder")
	}
	lines := append([]string{"FROM " + b.baseImage}, b.instructions...)
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "sandbox-api") && !strings.Contains(text, "blaxel-ai/sandbox") {
		lines = append(lines, "COPY --from="+b.sandboxAPIImage+" /sandbox-api "+sandboxAPIPath)
	}
	// Read from the lines rather than tracked by Entrypoint, so one written
	// with DockerfileLines counts too.
	hasEntrypoint := slices.ContainsFunc(b.instructions, func(instruction string) bool {
		return slices.ContainsFunc(strings.Split(instruction, "\n"), entrypointLine.MatchString)
	})
	if !hasEntrypoint {
		lines = append(lines, "ENTRYPOINT "+jsonArray(sandboxAPIPath))
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// Workdir appends WORKDIR <path>.
func (b *ImageBuilder) Workdir(path string) *ImageBuilder {
	return b.with([]string{"WORKDIR " + path}, nil, singleLine("Workdir path", path))
}

// RunCommands appends RUN <command> for each command, in shell form.
func (b *ImageBuilder) RunCommands(commands ...string) *ImageBuilder {
	lines := make([]string, 0, len(commands))
	errs := make([]error, 0, len(commands))
	for _, command := range commands {
		lines = append(lines, "RUN "+command)
		errs = append(errs, singleLine("RunCommands command", command))
	}
	return b.with(lines, nil, errs...)
}

// Env appends ENV <name>="<value>" for each variable, in name order, with "
// and \ in the value escaped. $ is left as is, so the build expands variables
// in it, as in {"PATH": "/opt/bin:$PATH"}.
func (b *ImageBuilder) Env(variables map[string]string) *ImageBuilder {
	return b.keyValues("ENV", "Env name", "Env value", variables)
}

// Label appends LABEL <key>="<value>" for each label, in key order, escaped
// as in [ImageBuilder.Env].
func (b *ImageBuilder) Label(labels map[string]string) *ImageBuilder {
	return b.keyValues("LABEL", "Label key", "Label value", labels)
}

// Copy appends COPY ["<source>", "<destination>"], copying from the build
// context.
func (b *ImageBuilder) Copy(source, destination string) *ImageBuilder {
	return b.with([]string{"COPY " + jsonArray(source, destination)}, nil,
		singleLine("Copy source", source), singleLine("Copy destination", destination))
}

// Expose appends EXPOSE <port> for each port.
func (b *ImageBuilder) Expose(ports ...int) *ImageBuilder {
	lines := make([]string, 0, len(ports))
	var errs []error
	for _, port := range ports {
		if port < 1 || port > 65535 {
			errs = append(errs, fmt.Errorf("Expose port %d is not between 1 and 65535", port))
		}
		lines = append(lines, "EXPOSE "+strconv.Itoa(port))
	}
	return b.with(lines, nil, errs...)
}

// Entrypoint appends ENTRYPOINT ["<arg>", ...], in exec form. The image then
// runs this instead of the sandbox API binary, so it must start that itself
// for the sandbox to be reachable.
func (b *ImageBuilder) Entrypoint(args ...string) *ImageBuilder {
	if len(args) == 0 {
		return b
	}
	errs := make([]error, 0, len(args))
	for _, arg := range args {
		errs = append(errs, singleLine("Entrypoint argument", arg))
	}
	return b.with([]string{"ENTRYPOINT " + jsonArray(args...)}, nil, errs...)
}

// User appends USER <user>.
func (b *ImageBuilder) User(user string) *ImageBuilder {
	return b.with([]string{"USER " + user}, nil, singleLine("User", user))
}

// Arg appends ARG <name>, or ARG <name>="<defaultValue>" when defaultValue is
// not empty, escaped as in [ImageBuilder.Env].
func (b *ImageBuilder) Arg(name, defaultValue string) *ImageBuilder {
	line := "ARG " + name
	if defaultValue != "" {
		line += "=" + quoted(defaultValue)
	}
	return b.with([]string{line}, nil, keyName("Arg name", name), singleLine("Arg default", defaultValue))
}

// DockerfileLines appends each line verbatim, for anything the other methods
// do not cover, such as comments, CMD, or a heredoc. A line may contain
// newlines, and nothing is checked or escaped.
func (b *ImageBuilder) DockerfileLines(lines ...string) *ImageBuilder {
	return b.with(lines, nil)
}

// PipInstall appends RUN pip install <args>.
func (b *ImageBuilder) PipInstall(args ...string) *ImageBuilder {
	return b.run("pip install", args)
}

// AptInstall appends RUN apt-get update && apt-get install -y
// --no-install-recommends <args> && rm -rf /var/lib/apt/lists/*, as one step
// so the package lists add nothing to the image.
func (b *ImageBuilder) AptInstall(args ...string) *ImageBuilder {
	if len(args) == 0 {
		return b
	}
	joined, err := shellArgs("AptInstall", args)
	return b.with([]string{
		"RUN apt-get update && apt-get install -y --no-install-recommends " + joined + " && rm -rf /var/lib/apt/lists/*",
	}, nil, err)
}

// ApkAdd appends RUN apk add --no-cache <args>.
func (b *ImageBuilder) ApkAdd(args ...string) *ImageBuilder {
	return b.run("apk add --no-cache", args)
}

// NpmInstall appends RUN npm install <args>, or RUN npm install with no
// arguments to install from the working directory's package.json.
func (b *ImageBuilder) NpmInstall(args ...string) *ImageBuilder {
	if len(args) == 0 {
		return b.with([]string{"RUN npm install"}, nil)
	}
	return b.run("npm install", args)
}

// GemInstall appends RUN gem install --no-document <args>.
func (b *ImageBuilder) GemInstall(args ...string) *ImageBuilder {
	return b.run("gem install --no-document", args)
}

// CargoInstall appends RUN cargo install <args>.
func (b *ImageBuilder) CargoInstall(args ...string) *ImageBuilder {
	return b.run("cargo install", args)
}

// GoInstall appends RUN go install <package> && go install <package> ..., one
// go install per package, since one call can only install packages from a
// single module.
func (b *ImageBuilder) GoInstall(packages ...string) *ImageBuilder {
	return b.runEach("go install", packages)
}

// ComposerInstall appends RUN composer require <args>.
func (b *ImageBuilder) ComposerInstall(args ...string) *ImageBuilder {
	return b.run("composer require", args)
}

// UvInstall appends RUN uv pip install --system <args>.
func (b *ImageBuilder) UvInstall(args ...string) *ImageBuilder {
	return b.run("uv pip install --system", args)
}

// PipxInstall appends RUN pipx install <package> && pipx install <package>
// ..., one per package.
func (b *ImageBuilder) PipxInstall(packages ...string) *ImageBuilder {
	return b.runEach("pipx install", packages)
}

// ImageBuilderAddFileOptions are the options for
// [ImageBuilder.AddFileWithOptions].
type ImageBuilderAddFileOptions struct {
	// Destination is the path in the image. Required.
	Destination string

	// Content is the file's content.
	Content []byte

	// ContextName is the file's name in the build context. Empty uses the
	// destination's base name, with a suffix such as " (1)" if another entry
	// already has that name.
	ContextName string

	// Mode is the file's permission bits in the image. Zero uses 0644.
	Mode fs.FileMode
}

// AddFile adds a file with the given content to the build context and appends
// COPY ["<name>", "<destination>"], where name is its name in the context. See
// [ImageBuilder.AddFileWithOptions] for more options.
func (b *ImageBuilder) AddFile(destination string, content []byte) *ImageBuilder {
	return b.AddFileWithOptions(ImageBuilderAddFileOptions{Destination: destination, Content: content})
}

// AddFileWithOptions adds a file with the given content to the build context
// and appends COPY ["<name>", "<destination>"], where name is its name in the
// context.
func (b *ImageBuilder) AddFileWithOptions(opts ImageBuilderAddFileOptions) *ImageBuilder {
	if opts.Destination == "" {
		return b.with(nil, nil, errors.New("AddFile Destination is required"))
	}
	if err := singleLine("AddFile Destination", opts.Destination); err != nil {
		return b.with(nil, nil, err)
	}
	defaultName := opts.Destination[strings.LastIndex(opts.Destination, "/")+1:]
	if opts.ContextName == "" && defaultName == "" {
		return b.with(nil, nil, fmt.Errorf("AddFile Destination %s has no file name; set ContextName", opts.Destination))
	}
	name, err := b.contextName(opts.ContextName, defaultName)
	if err != nil {
		return b.with(nil, nil, err)
	}
	// Copied, so changing the caller's slice later does not change the
	// builder.
	content := slices.Clone(opts.Content)
	if content == nil {
		content = []byte{}
	}
	return b.with([]string{"COPY " + jsonArray(name, opts.Destination)},
		&imageContextEntry{name: name, content: content, mode: cmp.Or(opts.Mode, defaultAddFileMode)})
}

// ImageBuilderAddLocalFileOptions are the options for
// [ImageBuilder.AddLocalFileWithOptions].
type ImageBuilderAddLocalFileOptions struct {
	// Source is the local file's path. Required.
	Source string

	// Destination is the path in the image. Required.
	Destination string

	// ContextName is the file's name in the build context. Empty uses the
	// source's base name, with a suffix such as " (1)" if another entry
	// already has that name.
	ContextName string
}

// AddLocalFile adds a local file to the build context and appends COPY
// ["<name>", "<destination>"], where name is its name in the context. The
// file is read when pushed, keeping its permissions, and a symbolic link is
// read as the file it points to. See [ImageBuilder.AddLocalFileWithOptions]
// for more options.
func (b *ImageBuilder) AddLocalFile(source, destination string) *ImageBuilder {
	return b.AddLocalFileWithOptions(ImageBuilderAddLocalFileOptions{Source: source, Destination: destination})
}

// AddLocalFileWithOptions adds a local file to the build context and appends
// COPY ["<name>", "<destination>"], where name is its name in the context.
// The file is read when pushed, keeping its permissions, and a symbolic link
// is read as the file it points to.
func (b *ImageBuilder) AddLocalFileWithOptions(opts ImageBuilderAddLocalFileOptions) *ImageBuilder {
	return b.addLocal("AddLocalFile", false, opts.Source, opts.Destination, opts.ContextName)
}

// ImageBuilderAddLocalDirOptions are the options for
// [ImageBuilder.AddLocalDirWithOptions].
type ImageBuilderAddLocalDirOptions struct {
	// Source is the local directory's path. Required.
	Source string

	// Destination is the path in the image. Required.
	Destination string

	// ContextName is the directory's name in the build context. Empty uses
	// the source's base name, with a suffix such as " (1)" if another entry
	// already has that name.
	ContextName string
}

// AddLocalDir adds a local directory to the build context and appends COPY
// ["<name>", "<destination>"], where name is its name in the context. As with
// any COPY of a directory, its contents are copied into the destination, not
// the directory itself. It is read when pushed, with files keeping their
// permissions, a link to a file within the directory stored as a copy of the
// file, a link to a file outside it failing the push, and a link to a
// directory stored as an empty directory. Everything in it is added: no
// .dockerignore or default ignore rules apply. See
// [ImageBuilder.AddLocalDirWithOptions] for more options.
func (b *ImageBuilder) AddLocalDir(source, destination string) *ImageBuilder {
	return b.AddLocalDirWithOptions(ImageBuilderAddLocalDirOptions{Source: source, Destination: destination})
}

// AddLocalDirWithOptions adds a local directory to the build context and
// appends COPY ["<name>", "<destination>"], where name is its name in the
// context. As with any COPY of a directory, its contents are copied into the
// destination, not the directory itself. It is read when pushed, with files
// keeping their permissions, a link to a file within the directory stored as
// a copy of the file, a link to a file outside it failing the push, and a
// link to a directory stored as an empty directory. Everything in it is
// added: no .dockerignore or default ignore rules apply.
func (b *ImageBuilder) AddLocalDirWithOptions(opts ImageBuilderAddLocalDirOptions) *ImageBuilder {
	return b.addLocal("AddLocalDir", true, opts.Source, opts.Destination, opts.ContextName)
}

func (b *ImageBuilder) addLocal(method string, dir bool, source, destination, contextName string) *ImageBuilder {
	var errs []error
	if source == "" {
		errs = append(errs, fmt.Errorf("%s Source is required", method))
	}
	if destination == "" {
		errs = append(errs, fmt.Errorf("%s Destination is required", method))
	}
	errs = append(errs, singleLine(method+" Destination", destination))
	if err := errors.Join(errs...); err != nil {
		return b.with(nil, nil, err)
	}
	// Resolved now, so a later change of working directory does not change
	// what is pushed.
	resolved, err := filepath.Abs(source)
	if err != nil {
		return b.with(nil, nil, err)
	}
	name, err := b.contextName(contextName, filepath.Base(resolved))
	if err != nil {
		return b.with(nil, nil, err)
	}
	return b.with([]string{"COPY " + jsonArray(name, destination)},
		&imageContextEntry{name: name, sourcePath: resolved, dir: dir})
}

// with returns a copy of the builder with lines and entry appended, or, if any
// of errs is not nil, with only the errors recorded. The slices are clipped
// before appending, so builders sharing a base never write into each other's
// arrays.
func (b *ImageBuilder) with(lines []string, entry *imageContextEntry, errs ...error) *ImageBuilder {
	next := *b
	if err := errors.Join(errs...); err != nil {
		next.errs = append(slices.Clip(b.errs), err)
		return &next
	}
	if len(lines) == 0 && entry == nil {
		return b
	}
	next.instructions = append(slices.Clip(b.instructions), lines...)
	if entry != nil {
		next.context = append(slices.Clip(b.context), *entry)
	}
	return &next
}

func (b *ImageBuilder) keyValues(instruction, keyWhat, valueWhat string, values map[string]string) *ImageBuilder {
	keys := slices.Sorted(maps.Keys(values))
	lines := make([]string, 0, len(keys))
	var errs []error
	for _, key := range keys {
		lines = append(lines, instruction+" "+key+"="+quoted(values[key]))
		errs = append(errs, keyName(keyWhat, key), singleLine(valueWhat, values[key]))
	}
	return b.with(lines, nil, errs...)
}

func (b *ImageBuilder) run(command string, args []string) *ImageBuilder {
	if len(args) == 0 {
		return b
	}
	joined, err := shellArgs(command, args)
	return b.with([]string{"RUN " + command + " " + joined}, nil, err)
}

func (b *ImageBuilder) runEach(command string, packages []string) *ImageBuilder {
	if len(packages) == 0 {
		return b
	}
	runs := make([]string, 0, len(packages))
	var errs []error
	for _, pkg := range packages {
		joined, err := shellArgs(command, []string{pkg})
		runs = append(runs, command+" "+joined)
		errs = append(errs, err)
	}
	return b.with([]string{"RUN " + strings.Join(runs, " && ")}, nil, errs...)
}

// contextName picks an entry's name in the build context. An explicit name is
// used exactly or fails; a default one is suffixed the way browsers name a
// repeated download, before any extension.
func (b *ImageBuilder) contextName(requested, defaultName string) (string, error) {
	taken := func(name string) bool {
		return name == "Dockerfile" || name == ".dockerignore" ||
			slices.ContainsFunc(b.context, func(entry imageContextEntry) bool { return entry.name == name })
	}
	if requested != "" {
		if err := contextNameSegment(requested); err != nil {
			return "", err
		}
		if taken(requested) {
			return "", fmt.Errorf("context name %q is reserved or already used", requested)
		}
		return requested, nil
	}
	if err := contextNameSegment(defaultName); err != nil {
		return "", err
	}
	if !taken(defaultName) {
		return defaultName, nil
	}
	stem, extension := defaultName, ""
	if dot := strings.LastIndex(defaultName, "."); dot > 0 {
		stem, extension = defaultName[:dot], defaultName[dot:]
	}
	for n := 1; ; n++ {
		if candidate := fmt.Sprintf("%s (%d)%s", stem, n, extension); !taken(candidate) {
			return candidate, nil
		}
	}
}

// zipEntries is the build context to zip: the Dockerfile, then each added
// entry, with local ones checked now and read while zipping.
func (b *ImageBuilder) zipEntries(ctx context.Context) ([]zipEntry, error) {
	dockerfile, err := b.Dockerfile()
	if err != nil {
		return nil, err
	}
	entries := []zipEntry{{path: "Dockerfile", data: []byte(dockerfile), mode: 0o644}}
	for _, entry := range b.context {
		if entry.sourcePath == "" {
			entries = append(entries, zipEntry{path: entry.name, data: entry.content, mode: entry.mode})
			continue
		}
		info, err := os.Stat(entry.sourcePath)
		if err != nil {
			return nil, err
		}
		if entry.dir {
			if !info.IsDir() {
				return nil, fmt.Errorf("local directory %s is not a directory", entry.sourcePath)
			}
			entries = append(entries, zipEntry{path: entry.name, dir: true, mode: info.Mode().Perm()})
			// No ignore rules: adding a directory is an explicit choice of
			// its files, as Docker's COPY of a directory takes them all.
			if err := addDirectoryZipEntries(ctx, entry.sourcePath, entry.name+"/", nil, &entries); err != nil {
				return nil, err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("local file %s is not a file", entry.sourcePath)
		}
		entries = append(entries, zipEntry{path: entry.name, sourcePath: entry.sourcePath, mode: info.Mode().Perm()})
	}
	return entries, nil
}

// singleLine fails if a value would span lines in the Dockerfile.
func singleLine(what, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s must not contain a newline: %q", what, value)
	}
	return nil
}

// keyName checks an env, label, or arg name, which is written unquoted.
func keyName(what, value string) error {
	if value == "" || strings.ContainsAny(value, `="`) || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return fmt.Errorf(`%s %q must be non-empty, without spaces, = or "`, what, value)
	}
	return nil
}

var dockerfileQuoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// quoted double-quotes a value, escaping " and \.
func quoted(value string) string {
	return `"` + dockerfileQuoteEscaper.Replace(value) + `"`
}

// contextNameSegment fails unless name is one path segment.
func contextNameSegment(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\r\n") {
		return fmt.Errorf("context name %q must be one path segment, without slashes or newlines", name)
	}
	return nil
}

// shellArgs joins arguments for the shell, single-quoting any that need it.
func shellArgs(what string, args []string) (string, error) {
	quotedArgs := make([]string, 0, len(args))
	errs := make([]error, 0, len(args))
	for _, arg := range args {
		errs = append(errs, singleLine(what+" argument", arg))
		if shellSafe.MatchString(arg) {
			quotedArgs = append(quotedArgs, arg)
		} else {
			quotedArgs = append(quotedArgs, shellQuote(arg))
		}
	}
	return strings.Join(quotedArgs, " "), errors.Join(errs...)
}

// jsonArray is values as a JSON array, for the exec form of an instruction.
func jsonArray(values ...string) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	// Docker reads the array as JSON, so escaping < > & is only noise.
	encoder.SetEscapeHTML(false)
	// Encoding strings cannot fail.
	_ = encoder.Encode(values)
	return strings.TrimSuffix(buf.String(), "\n")
}
