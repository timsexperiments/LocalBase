#include <cassert>
#include <string>
#include <vector>

#include "require_gpu_pci.h"

int main() {
    const LocalBaseGpuDevice nvidia{"Vulkan1", "Vulkan", "0000:01:00.0", true};
    std::string backend;
    std::string error;

    assert(localbase_resolve_gpu_pci("0000:01:00.0", {nvidia}, backend, error));
    assert(backend == "Vulkan1");

    for (const std::string& invalid : {
             "00000000:01:00.0", "0000:AB:00.0", "0000:01:20.0", "0000:01:00.8"}) {
        backend.clear();
        error.clear();
        assert(!localbase_resolve_gpu_pci(invalid, {nvidia}, backend, error));
    }

    for (const std::vector<LocalBaseGpuDevice>& devices : {
             std::vector<LocalBaseGpuDevice>{},
             std::vector<LocalBaseGpuDevice>{{"Vulkan0", "Vulkan", "0000:01:00.0", false}},
             std::vector<LocalBaseGpuDevice>{{"CUDA0", "CUDA", "0000:01:00.0", true}},
             std::vector<LocalBaseGpuDevice>{nvidia, {"Vulkan2", "Vulkan", "0000:01:00.0", true}},
         }) {
        backend.clear();
        error.clear();
        assert(!localbase_resolve_gpu_pci("0000:01:00.0", devices, backend, error));
        assert(error.find("exactly one") != std::string::npos);
    }

    backend = "Vulkan0";
    error.clear();
    assert(!localbase_resolve_gpu_pci("0000:01:00.0", {nvidia}, backend, error));
    assert(error.find("conflicts") != std::string::npos);
}
