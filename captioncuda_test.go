package main

import (
	"reflect"
	"testing"
)

func TestCUDARuntimeRequirements(t *testing.T) {
	want := []string{"libcuda.so.1", "libcudart.so.12", "libcublasLt.so.12", "libcublas.so.12"}
	got := engineRequirements(engineVariant{Needs: cudaRuntime.Needs, AlsoNeeds: cudaRuntime.AlsoNeeds})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CUDA libraries = %v, want %v", got, want)
	}
}

func TestCUDARuntimePackages(t *testing.T) {
	want := []string{"libcudart12", "libcublas12"}
	if !reflect.DeepEqual(cudaRuntime.Packages, want) {
		t.Fatalf("CUDA packages = %v, want %v", cudaRuntime.Packages, want)
	}
}
