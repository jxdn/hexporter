package collectors

import "strings"

// Inference / training framework detection for GPU processes.
//
// The nvidia-smi process name is often just "python", so the process name,
// the full command line and, if nothing matches, up to 5 parent processes are
// checked against these rules. First match wins, so order matters.

type frameworkRule struct {
	Name     string
	Patterns []string // lower-case substrings
}

var frameworkRules = []frameworkRule{
	{"vllm", []string{"vllm", "rayworkerwrapper"}},
	{"sglang", []string{"sglang"}},
	{"tgi", []string{"text-generation-launcher", "text-generation-server", "text_generation_server"}},
	{"triton", []string{"tritonserver"}},
	{"tensorrt-llm", []string{"trtllm", "tensorrt_llm", "tensorrt-llm"}},
	{"lmdeploy", []string{"lmdeploy"}},
	{"ollama", []string{"ollama"}},
	{"llama.cpp", []string{"llama-server", "llama_cpp", "llama.cpp"}},
	{"deepspeed-mii", []string{"deepspeed.mii", "mii.serve"}},
	{"ray-serve", []string{"ray::serve", "serve run"}},
	{"jupyter", []string{"jupyter", "ipykernel"}},
	{"training", []string{"torchrun", "deepspeed", "accelerate launch", "torch.distributed", "megatron"}},
}

// classifyFramework returns the framework for a text, or "other".
func classifyFramework(text string) string {
	t := strings.ToLower(text)
	for _, r := range frameworkRules {
		for _, p := range r.Patterns {
			if strings.Contains(t, p) {
				return r.Name
			}
		}
	}
	return "other"
}

// frameworkForPID classifies a GPU process by its nvidia-smi name, its own
// name and command line, then up to 5 parents.
func frameworkForPID(pid int, smiName string) string {
	fw := classifyFramework(smiName + " " + procComm(pid) + " " + procCmdline(pid))
	cur := pid
	for i := 0; i < 5 && fw == "other"; i++ {
		cur = procPPID(cur)
		if cur <= 1 {
			break
		}
		fw = classifyFramework(procComm(cur) + " " + procCmdline(cur))
	}
	return fw
}
