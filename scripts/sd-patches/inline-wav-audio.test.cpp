#include <cassert>
#include <cmath>
#include <cstdint>
#include <cstring>
#include <limits>
#include <string>
#include <utility>
#include <vector>

#include "inline_wav.h"

namespace {

void append_u16(std::vector<uint8_t>& bytes, uint16_t value) {
    bytes.push_back(static_cast<uint8_t>(value));
    bytes.push_back(static_cast<uint8_t>(value >> 8));
}

void append_u32(std::vector<uint8_t>& bytes, uint32_t value) {
    bytes.push_back(static_cast<uint8_t>(value));
    bytes.push_back(static_cast<uint8_t>(value >> 8));
    bytes.push_back(static_cast<uint8_t>(value >> 16));
    bytes.push_back(static_cast<uint8_t>(value >> 24));
}

std::string base64(const std::vector<uint8_t>& bytes) {
    static constexpr char alphabet[] =
        "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    std::string encoded;
    for (size_t offset = 0; offset < bytes.size(); offset += 3) {
        const size_t remaining = bytes.size() - offset;
        const uint32_t word = static_cast<uint32_t>(bytes[offset]) << 16 |
                              static_cast<uint32_t>(remaining > 1 ? bytes[offset + 1] : 0) << 8 |
                              static_cast<uint32_t>(remaining > 2 ? bytes[offset + 2] : 0);
        encoded.push_back(alphabet[(word >> 18) & 63]);
        encoded.push_back(alphabet[(word >> 12) & 63]);
        encoded.push_back(remaining > 1 ? alphabet[(word >> 6) & 63] : '=');
        encoded.push_back(remaining > 2 ? alphabet[word & 63] : '=');
    }
    return encoded;
}

std::vector<uint8_t> wav(uint16_t format,
                         uint16_t channels,
                         uint32_t sample_rate,
                         uint16_t bits,
                         const std::vector<uint8_t>& samples) {
    std::vector<uint8_t> bytes;
    bytes.insert(bytes.end(), {'R', 'I', 'F', 'F'});
    append_u32(bytes, static_cast<uint32_t>(36 + samples.size()));
    bytes.insert(bytes.end(), {'W', 'A', 'V', 'E', 'f', 'm', 't', ' '});
    append_u32(bytes, 16);
    append_u16(bytes, format);
    append_u16(bytes, channels);
    append_u32(bytes, sample_rate);
    const uint16_t align = channels * bits / 8;
    append_u32(bytes, sample_rate * align);
    append_u16(bytes, align);
    append_u16(bytes, bits);
    bytes.insert(bytes.end(), {'d', 'a', 't', 'a'});
    append_u32(bytes, static_cast<uint32_t>(samples.size()));
    bytes.insert(bytes.end(), samples.begin(), samples.end());
    return bytes;
}

nlohmann::json input(const std::vector<uint8_t>& bytes) {
    return {{"format", "wav"}, {"data", base64(bytes)}};
}

}  // namespace

int main() {
    std::string error;
    SDAudioOwner audio;

    const std::vector<uint8_t> pcm = {0x00, 0x80, 0x00, 0x00, 0xff, 0x7f};
    assert(parse_inline_wav(input(wav(1, 1, 8000, 16, pcm)), audio, error));
    auto view = audio.get();
    assert(view.sample_rate == 8000);
    assert(view.channels == 1);
    assert(view.sample_count == 3);
    assert(view.data[0] == -1.0f);
    assert(view.data[1] == 0.0f);
    assert(view.data[2] > 0.99f);

    std::vector<SDAudioOwner> job_owned;
    job_owned.push_back(std::move(audio));
    assert(job_owned.front().get().sample_count == 3);
    assert(job_owned.front().get().data[0] == -1.0f);

    const std::vector<float> float_values = {-1.0f, -0.25f, 0.5f, 1.0f};
    std::vector<uint8_t> float_samples(float_values.size() * sizeof(float));
    std::memcpy(float_samples.data(), float_values.data(), float_samples.size());
    error.clear();
    assert(parse_inline_wav(input(wav(3, 2, 16000, 32, float_samples)), audio, error));
    view = audio.get();
    assert(view.sample_rate == 16000);
    assert(view.channels == 2);
    assert(view.sample_count == 2);
    for (size_t index = 0; index < float_values.size(); ++index) {
        assert(view.data[index] == float_values[index]);
    }

    error.clear();
    assert(!parse_inline_wav("/tmp/client.wav", audio, error));
    assert(error.find("audio must be") != std::string::npos);

    error.clear();
    auto extra_field = input(wav(1, 1, 8000, 16, pcm));
    extra_field["url"] = "https://example.invalid/audio.wav";
    assert(!parse_inline_wav(extra_field, audio, error));

    error.clear();
    assert(!parse_inline_wav(input(wav(1, 3, 8000, 16, pcm)), audio, error));
    assert(error.find("one or two channels") != std::string::npos);

    error.clear();
    assert(!parse_inline_wav(input(wav(1, 1, 7999, 16, pcm)), audio, error));
    assert(error.find("8000 and 192000") != std::string::npos);

    error.clear();
    assert(!parse_inline_wav(input(wav(1, 1, 16000, 24, {0, 0, 0, 0, 0, 0})), audio, error));
    assert(error.find("PCM16 or IEEE float32") != std::string::npos);

    error.clear();
    std::vector<uint8_t> too_long((8000 * 30 + 1) * 2);
    assert(!parse_inline_wav(input(wav(1, 1, 8000, 16, too_long)), audio, error));
    assert(error.find("30 seconds") != std::string::npos);

    float nan = std::numeric_limits<float>::quiet_NaN();
    std::vector<uint8_t> float_sample(sizeof(float));
    std::memcpy(float_sample.data(), &nan, sizeof(float));
    error.clear();
    assert(!parse_inline_wav(input(wav(3, 1, 16000, 32, float_sample)), audio, error));
    assert(error.find("finite") != std::string::npos);

    for (float amplitude : {1.1f, -1.1f}) {
        std::memcpy(float_sample.data(), &amplitude, sizeof(float));
        error.clear();
        assert(!parse_inline_wav(input(wav(3, 1, 16000, 32, float_sample)), audio, error));
        assert(error.find("normalized") != std::string::npos);
    }

    auto trailing = wav(1, 1, 8000, 16, pcm);
    const uint32_t shorter_riff = static_cast<uint32_t>(trailing.size() - 10);
    std::memcpy(trailing.data() + 4, &shorter_riff, sizeof(shorter_riff));
    error.clear();
    assert(!parse_inline_wav(input(trailing), audio, error));
    assert(error.find("RIFF size") != std::string::npos);

    auto partial_header = wav(1, 1, 8000, 16, pcm);
    partial_header.push_back('J');
    partial_header.push_back('U');
    partial_header.push_back('N');
    const uint32_t partial_size = static_cast<uint32_t>(partial_header.size() - 8);
    std::memcpy(partial_header.data() + 4, &partial_size, sizeof(partial_size));
    error.clear();
    assert(!parse_inline_wav(input(partial_header), audio, error));
    assert(error.find("partial chunk header") != std::string::npos);

    auto missing_pad = wav(1, 1, 8000, 16, pcm);
    missing_pad.insert(missing_pad.end(), {'J', 'U', 'N', 'K'});
    append_u32(missing_pad, 1);
    missing_pad.push_back(0);
    const uint32_t missing_pad_size = static_cast<uint32_t>(missing_pad.size() - 8);
    std::memcpy(missing_pad.data() + 4, &missing_pad_size, sizeof(missing_pad_size));
    error.clear();
    assert(!parse_inline_wav(input(missing_pad), audio, error));
    assert(error.find("padding byte") != std::string::npos);

    error.clear();
    nlohmann::json malformed = {{"format", "wav"}, {"data", "AAAA=A=="}};
    assert(!parse_inline_wav(malformed, audio, error));
    assert(error.find("canonical base64") != std::string::npos);

    error.clear();
    nlohmann::json oversized = {
        {"format", "wav"},
        {"data", std::string(((INLINE_WAV_MAX_BYTES + 2) / 3) * 4 + 4, 'A')}};
    assert(!parse_inline_wav(oversized, audio, error));
    assert(error.find("size limit") != std::string::npos ||
           error.find("24 MiB") != std::string::npos);
}
