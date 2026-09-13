#include "whisper.h"
#include "require-gpu-pci.h"
#include <cassert>
#include <stdexcept>
#include <vector>

struct ggml_backend_reg { const char * name; };
struct ggml_backend_device {
    enum ggml_backend_dev_type type;
    const char * pci;
    ggml_backend_reg_t reg;
};
static ggml_backend_reg vulkan = {"Vulkan"};
static ggml_backend_reg cuda = {"CUDA"};
static std::vector<ggml_backend_device> devices;
static bool gpu_initializes;
static int cpu_initializations;
static int accel_initializations;
static int fake_backend;

size_t ggml_backend_dev_count() { return devices.size(); }
ggml_backend_dev_t ggml_backend_dev_get(size_t i) { return &devices.at(i); }
enum ggml_backend_dev_type ggml_backend_dev_type(ggml_backend_dev_t dev) { return dev->type; }
void ggml_backend_dev_get_props(ggml_backend_dev_t dev, ggml_backend_dev_props * props) { props->device_id = dev->pci; }
ggml_backend_reg_t ggml_backend_dev_backend_reg(ggml_backend_dev_t dev) { return dev->reg; }
const char * ggml_backend_reg_name(ggml_backend_reg_t reg) { return reg->name; }
const char * ggml_backend_dev_name(ggml_backend_dev_t) { return "test"; }
ggml_backend_t ggml_backend_dev_init(ggml_backend_dev_t, const char *) {
    ++accel_initializations;
    return reinterpret_cast<ggml_backend_t>(&fake_backend);
}
ggml_backend_t ggml_backend_init_by_type(enum ggml_backend_dev_type type, const char *) {
    assert(type == GGML_BACKEND_DEVICE_TYPE_CPU);
    ++cpu_initializations;
    return reinterpret_cast<ggml_backend_t>(&fake_backend);
}
static ggml_backend_t whisper_backend_init_gpu(const whisper_context_params & params) {
    return params.use_gpu && gpu_initializes ? reinterpret_cast<ggml_backend_t>(&fake_backend) : nullptr;
}
#define WHISPER_LOG_INFO(...) ((void) 0)
#define WHISPER_LOG_ERROR(...) ((void) 0)
// Extracted verbatim from the patched, checksum-verified source by the CI runner.
#include "whisper-backend-init.inc"

int main() {
    const std::string target = "0000:ab:1f.7";
    assert(whisper_valid_gpu_pci(target));
    assert(whisper_valid_gpu_pci("ffff:ff:00.0"));
    for (const auto invalid : {"", "00000000:ab:1f.7", "0000:AB:1F.7", "0000:ab:20.0",
            "0000:ab:1f.8", "0000:ab:1f.-", "0000:ab:1f.0extra", "0000:ab:1g.0", "0000-ab:1f.0"}) {
        assert(!whisper_valid_gpu_pci(invalid));
        assert(whisper_find_gpu_pci(invalid) == -1);
    }
    assert(whisper_find_gpu_pci(target) == -1);
    devices = {
        {GGML_BACKEND_DEVICE_TYPE_CPU, nullptr, &cuda},
        {GGML_BACKEND_DEVICE_TYPE_GPU, target.c_str(), &cuda},
        {GGML_BACKEND_DEVICE_TYPE_IGPU, "0000:00:02.0", &vulkan},
        {GGML_BACKEND_DEVICE_TYPE_ACCEL, nullptr, &cuda},
        {GGML_BACKEND_DEVICE_TYPE_GPU, target.c_str(), &vulkan},
    };
    assert(whisper_find_gpu_pci(target) == 2);
    std::swap(devices[1], devices[4]);
    assert(whisper_find_gpu_pci(target) == 0);
    devices.push_back({GGML_BACKEND_DEVICE_TYPE_GPU, target.c_str(), &vulkan});
    assert(whisper_find_gpu_pci(target) == -1);
    devices = {{GGML_BACKEND_DEVICE_TYPE_GPU, nullptr, &vulkan}};
    assert(whisper_find_gpu_pci(target) == -1);
    devices = {{GGML_BACKEND_DEVICE_TYPE_GPU, "0000:ab:1e.7", &vulkan}};
    assert(whisper_find_gpu_pci(target) == -1);
    devices = {{GGML_BACKEND_DEVICE_TYPE_GPU, target.c_str(), &cuda}};
    assert(whisper_find_gpu_pci(target) == -1);
    devices = {{GGML_BACKEND_DEVICE_TYPE_IGPU, target.c_str(), &vulkan}};
    assert(whisper_find_gpu_pci(target) == -1);

    devices = {{GGML_BACKEND_DEVICE_TYPE_ACCEL, nullptr, &cuda}};
    whisper_context_params params = {};
    params.use_gpu = true;
    params.require_gpu = true;
    gpu_initializes = false;
    assert(whisper_backend_init(params).empty());
    assert(cpu_initializations == 0 && accel_initializations == 0);
    gpu_initializes = true;
    assert(whisper_backend_init(params).size() == 3);
    assert(cpu_initializations == 1 && accel_initializations == 1);
    params.use_gpu = false;
    assert(whisper_backend_init(params).empty());
    assert(cpu_initializations == 1 && accel_initializations == 1);
    params.require_gpu = false;
    assert(whisper_backend_init(params).size() == 2);
    assert(cpu_initializations == 2 && accel_initializations == 2);
}
