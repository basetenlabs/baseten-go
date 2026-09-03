module github.com/basetenlabs/baseten-go/internal/separate-module-tests

go 1.26.1

replace github.com/basetenlabs/baseten-go => ../..

require github.com/basetenlabs/baseten-go v0.0.0-00010101000000-000000000000

require (
	github.com/klauspost/compress v1.17.11
	github.com/zeebo/blake3 v0.2.4
	golang.org/x/sys v0.47.0
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/klauspost/cpuid/v2 v2.0.12 // indirect
