// Local 16 kHz signed PCM to Whisper's normalized, mel-major input tensor.
#pragma once
#include <algorithm>
#include <array>
#include <cmath>
#include <cstdint>
#include <fstream>
#include <stdexcept>
#include <utility>
#include <vector>

extern "C" {
#include <libavutil/tx.h>
}

class WhisperFeatures {
  static constexpr int window = 400, hop = 160;
  std::ifstream pcm;
  int64_t samples;
  std::vector<std::vector<std::pair<int, float>>> filters;
  std::array<float, window> hann, frame;
  std::array<AVComplexFloat, window / 2 + 1> spectrum;
  AVTXContext *fft = nullptr;
  av_tx_fn transform = nullptr;
  float floor_log = -10.f;

  std::vector<float> extract(int64_t start, int count) {
    const int64_t offset = start * hop - window / 2;
    const int64_t first = std::max<int64_t>(0, offset);
    const int64_t last = std::min(samples, offset + (count - 1) * hop + window);
    // The initial centered window reflects real samples. The right edge
    // includes the frontend's extra 160 zero samples before reflection.
    std::vector<int16_t> buffer(size_t(std::max<int64_t>(0, last - first)));
    pcm.clear();
    pcm.seekg(first * 2);
    if (!pcm.read(reinterpret_cast<char *>(buffer.data()), buffer.size() * 2))
      throw std::runtime_error("PCM read");
    std::vector<float> output(filters.size() * count);
    for (int t = 0; t < count; ++t) {
      for (int k = 0; k < window; ++k) {
        int64_t i = std::abs(offset + t * hop + k);
        if (i >= samples + hop)
          i = 2 * (samples + hop - 1) - i;
        frame[k] = (i >= first && i < last ? buffer[size_t(i - first)] / 32768.f
                                           : 0.f) *
                   hann[k];
      }
      transform(fft, spectrum.data(), frame.data(), sizeof(float));
      std::array<float, window / 2 + 1> power;
      for (size_t i = 0; i < power.size(); ++i)
        power[i] =
            spectrum[i].re * spectrum[i].re + spectrum[i].im * spectrum[i].im;
      for (size_t m = 0; m < filters.size(); ++m) {
        float sum = 0;
        for (const auto &[bin, weight] : filters[m])
          sum += power[bin] * weight;
        output[m * count + t] = std::log10(std::max(sum, 1e-10f));
      }
    }
    return output;
  }

public:
  static constexpr int chunk = 3000;
  int64_t frames() const { return samples / hop; }
  WhisperFeatures(const char *input, size_t bands)
      : pcm(input, std::ios::binary | std::ios::ate), filters(bands) {
    const auto bytes = pcm.tellg();
    if (bytes < 2 || bytes % 2 || (bands != 80 && bands != 128))
      throw std::runtime_error("PCM or mel bands");
    samples = bytes / 2;
    for (int i = 0; i < window; ++i)
      hann[i] = float(.5 - .5 * std::cos(2 * std::acos(-1.) * i / window));
    std::vector<double> edges(bands + 2);
    for (size_t i = 0; i < edges.size(); ++i) {
      const double mel = 45.245640471924965 * i / (bands + 1);
      edges[i] = mel < 15 ? mel * (200. / 3)
                          : 1000 * std::exp((mel - 15) * std::log(6.4) / 27);
    }
    for (size_t m = 0; m < bands; ++m)
      for (int k = 0; k <= window / 2; ++k) {
        double hz = k * 40;
        double w =
            std::min((hz - edges[m]) / (edges[m + 1] - edges[m]),
                     (edges[m + 2] - hz) / (edges[m + 2] - edges[m + 1]));
        if (w > 0)
          filters[m].emplace_back(k, float(w * 2 / (edges[m + 2] - edges[m])));
      }
    float scale = 1;
    if (av_tx_init(&fft, &transform, AV_TX_FLOAT_RDFT, 0, window, &scale,
                   AV_TX_UNALIGNED) < 0)
      throw std::runtime_error("FFT initialization");
  }
  ~WhisperFeatures() { av_tx_uninit(&fft); }
  void measure_peak() {
    float peak = -10;
    for (int64_t pos = 0; pos <= frames(); pos += chunk) {
      auto values =
          extract(pos, int(std::min<int64_t>(chunk, frames() + 1 - pos)));
      peak = std::max(peak, *std::max_element(values.begin(), values.end()));
    }
    floor_log = peak - 8;
  }
  std::vector<float> window_at(int64_t start, int count) {
    auto values = extract(start, count);
    std::vector<float> padded(filters.size() * chunk, 0.f);
    for (size_t m = 0; m < filters.size(); ++m)
      for (int i = 0; i < count; ++i)
        padded[m * chunk + i] =
            (std::max(values[m * count + i], floor_log) + 4.f) / 4.f;
    return padded;
  }
};
