package main

// transcribe.cpp's CUDA build carries the CUDA backend, but not NVIDIA's CUDA
// runtime or cuBLAS. Debian packages those libraries separately from the host
// driver that the NVIDIA container runtime injects.
var cudaRuntimeLibraries = []string{
	"libcudart.so.12",
	"libcublasLt.so.12",
	"libcublas.so.12",
}

var cudaRuntime = gpuRuntime{
	Key:       "cuda",
	Name:      "CUDA runtime",
	Desc:      "The CUDA 12 runtime and cuBLAS libraries used by the CUDA caption engine. The NVIDIA container runtime still supplies the host driver.",
	Packages:  []string{"libcudart12", "libcublas12"},
	Needs:     "libcuda.so.1",
	AlsoNeeds: cudaRuntimeLibraries,
	Note:      "Your compose file also has to use the NVIDIA container runtime and expose the GPU with compute and utility capabilities.",
}
