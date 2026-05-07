package separatemoduletests_test

import (
	"testing"

	"github.com/basetenlabs/baseten-go/client/modelconfig"
	"github.com/basetenlabs/baseten-go/internal/require"
	"gopkg.in/yaml.v3"
)

func unmarshalConfig(t *testing.T, src string) modelconfig.ModelConfig {
	t.Helper()
	var cfg modelconfig.ModelConfig
	require.NoError(t, yaml.Unmarshal([]byte(src), &cfg))
	return cfg
}

func TestVLLMConfig(t *testing.T) {
	// From truss-examples/vllm/config.yaml
	cfg := unmarshalConfig(t, `
model_name: "Llama 3.1 8B Instruct VLLM openai compatible"
python_version: py311
model_metadata:
  example_model_input: {"prompt": "what is the meaning of life"}
  repo_id: meta-llama/Llama-3.1-8B-Instruct
  openai_compatible: true
requirements:
  - vllm==0.5.4
resources:
  accelerator: A100
  use_gpu: true
runtime:
  predict_concurrency: 128
secrets:
  hf_access_token: null
`)
	require.NotNil(t, cfg.ModelName)
	require.Equal(t, "Llama 3.1 8B Instruct VLLM openai compatible", *cfg.ModelName)
	require.Equal(t, "py311", cfg.PythonVersion)
	require.Len(t, cfg.Requirements, 1)
	require.Equal(t, "vllm==0.5.4", cfg.Requirements[0])
	require.NotNil(t, cfg.Resources)
	require.NotNil(t, cfg.Resources.Accelerator)
	require.Equal(t, "A100", string(*cfg.Resources.Accelerator))
	require.NotNil(t, cfg.Runtime)
	require.Equal(t, 128, cfg.Runtime.PredictConcurrency)
}

func TestWhisperConfig(t *testing.T) {
	// From truss-examples/07-high-performance-dynamic-batching/config.yaml
	cfg := unmarshalConfig(t, `
base_image:
  image: baseten/trtllm-server:r23.12_baseten_v0.9.0.dev2024022000
  python_executable_path: /usr/bin/python3
model_name: TRT Whisper - Dynamic Batching
python_version: py311
model_cache:
  - repo_id: baseten/trtllm-whisper-a10g-large-v2-1
    revision: main
    use_volume: true
    volume_folder: trtllm-whisper-a10g-large-v2-1
resources:
  accelerator: A10G
runtime:
  predict_concurrency: 256
external_data:
  - local_data_path: assets/multilingual.tiktoken
    url: https://raw.githubusercontent.com/openai/whisper/main/whisper/assets/multilingual.tiktoken
`)
	require.NotNil(t, cfg.ModelName)
	require.Equal(t, "TRT Whisper - Dynamic Batching", *cfg.ModelName)
	require.NotNil(t, cfg.BaseImage)
	require.Equal(t, "baseten/trtllm-server:r23.12_baseten_v0.9.0.dev2024022000", cfg.BaseImage.Image)
	require.Len(t, cfg.ModelCache, 1)
	require.Equal(t, "baseten/trtllm-whisper-a10g-large-v2-1", cfg.ModelCache[0].RepoId)
	require.True(t, cfg.ModelCache[0].UseVolume, "use_volume")
	require.Len(t, cfg.ExternalData, 1)
	require.Equal(t, "assets/multilingual.tiktoken", cfg.ExternalData[0].LocalDataPath)
}

func TestChatterboxConfig(t *testing.T) {
	// From truss-examples/chatterbox-tts/config.yaml
	cfg := unmarshalConfig(t, `
model_name: Chatterbox TTS
base_image:
  image: jojobaseten/truss-numpy-1.26.0-gpu:0.4
  python_executable_path: /usr/bin/python3
python_version: py312
requirements:
  - chatterbox-tts
resources:
  accelerator: H100
  cpu: '1'
  memory: 40Gi
  use_gpu: true
`)
	require.NotNil(t, cfg.ModelName)
	require.Equal(t, "Chatterbox TTS", *cfg.ModelName)
	require.Equal(t, "py312", cfg.PythonVersion)
	require.NotNil(t, cfg.Resources)
	require.NotNil(t, cfg.Resources.Accelerator)
	require.Equal(t, "H100", string(*cfg.Resources.Accelerator))
	require.Equal(t, "1", cfg.Resources.Cpu)
	require.Equal(t, "40Gi", cfg.Resources.Memory)
}
