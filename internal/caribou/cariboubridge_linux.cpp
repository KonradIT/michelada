#define _USE_MATH_DEFINES
#include <iostream>
#include <string>
#include <CaribouLite.hpp>
#include <thread>
#include <complex>
#include <cmath>
#include <vector>


// get driver instance - use "CaribouLite&" rather than "CaribouLite" (ref)
CaribouLite &cl = CaribouLite::GetInstance();

void _info ()
{
    std::cout << "CaribouLite Bridge: calling info()" << std::endl;
    // get the radios
    CaribouLiteRadio *s1g = cl.GetRadioChannel(CaribouLiteRadio::RadioType::S1G);
    CaribouLiteRadio *hif = cl.GetRadioChannel(CaribouLiteRadio::RadioType::HiF);

    // write radio information
    std::cout << "First Radio Name: " << s1g->GetRadioName() << "  MtuSize: " << std::dec << s1g->GetNativeMtuSample() << " Samples" << std::endl;
    std::cout << "First Radio Name: " << hif->GetRadioName() << "  MtuSize: " << std::dec << hif->GetNativeMtuSample() << " Samples" << std::endl;
    std::cout << "Initialized CaribouLite: " << cl.IsInitialized() << std::endl;
    std::cout << "API Versions: " << cl.GetApiVersion() << std::endl;
    std::cout << "Hardware Serial Number: " << std::hex << cl.GetHwSerialNumber() << std::endl;
    std::cout << "System Type: " << cl.GetSystemVersionStr() << std::endl;
    std::cout << "Hardware Unique ID: " << cl.GetHwGuid() << std::endl;
}

void _setFreq(int freq) {
    CaribouLiteRadio *s1g = cl.GetRadioChannel(CaribouLiteRadio::RadioType::S1G);

    try {
        s1g->SetFrequency(freq);
    } catch (...) {
        std::cout << "The specified freq couldn't be used" << std::endl;
    }
}

void _setRxGain(int gain) {
    CaribouLiteRadio *s1g = cl.GetRadioChannel(CaribouLiteRadio::RadioType::S1G);
    s1g->SetRxGain(gain);
}

void _setAgc(bool agc) {
    CaribouLiteRadio *s1g = cl.GetRadioChannel(CaribouLiteRadio::RadioType::S1G);
    s1g->SetAgc(agc);
}

// Calculate the RSSI
float _rssi(const std::complex<float>* signal, size_t num_of_samples)
{
    if (num_of_samples == 0)
    {
        return 0.0f;
    }

    float sum_of_squares = 0.0f;
    for (size_t i = 0; i < num_of_samples; ++i)
    {
        float vrmsp2 = (signal[i].real() * signal[i].real()) + (signal[i].imag() * signal[i].imag());
        sum_of_squares += vrmsp2 / 100.0;
    }

    float mean_of_squares = sum_of_squares / num_of_samples;

    // Convert RMS value to dBm
    return 10 * log10(mean_of_squares);
}

typedef void (*ReceiveData)(float rssi, float freq);
ReceiveData goCallback = nullptr;

void receivedSamples(CaribouLiteRadio* radio, const std::complex<float>* samples, CaribouLiteMeta* sync, size_t num_samples)
{
    if (goCallback) {
        goCallback(_rssi(samples, num_samples), radio->GetFrequency());
    }
}

void _readRssi(ReceiveData callback) {
    CaribouLiteRadio *s1g = cl.GetRadioChannel(CaribouLiteRadio::RadioType::S1G);
    goCallback = callback;

    try
    {
        s1g->SetFrequency(900000000);
    }
    catch (...)
    {
        std::cout << "The specified freq couldn't be used" << std::endl;
    }
    // Gain and AGC are configured by Go via SetS1GGain/SetS1GAGC before this call.
    // StartReceiving is non-blocking; the callback runs on a CaribouLite background thread.
    s1g->StartReceiving(receivedSamples);
}

void _stopRssi() {
    CaribouLiteRadio *s1g = cl.GetRadioChannel(CaribouLiteRadio::RadioType::S1G);
    s1g->StopReceiving();
    goCallback = nullptr;
}


// HiF Radio — FM demodulation for 5.8 GHz analog FPV (PAL) streams

// The FM discriminator computes the instantaneous phase difference between
// consecutive IQ samples:  demod[n] = arg(x[n] * conj(x[n-1]))
// Output is in radians; the PAL composite video signal rides on this.
//
// Bandwidth note: CaribouLite HiF delivers ~4 MSPS.  Typical FPV FM
// deviation is ±7-9 MHz, so the discriminator will alias at this sample
// rate.  The resulting video is low-fidelity but sync-detectable.



// IQ power-envelope demodulator
//
// Why not atan2 FM discriminator:
//   FPV analog video uses ~±7.5 MHz FM deviation.  At 4 MSPS the phase step
//   per sample is up to 2π×7.5/4 ≈ 11.8 rad — far beyond atan2's ±π wrap.
//   The discriminator output is completely aliased and carries no video info.
//
// Power-envelope (slope detection via IF filter edge):
//   The AT86RF215 IF passband is ~±2 MHz.  When the FM carrier swings outside
//   this window, received power drops sharply.  PAL sync pulses drive the
//   carrier to its most-deviated frequency, producing consistent power dips
//   that the PAL sync detector can latch onto.  Works *because* the deviation
//   is large, not despite it.


typedef void (*ReceiveVideoSamples)(float* samples, int num_samples);
static ReceiveVideoSamples videoCallback = nullptr;


// Carrier frequency correction + 64-sample moving-average LPF
//
// PSD analysis of a live capture showed the FPV carrier sits at -1.5 MHz
// offset from the AT86RF215 IF centre.  We compensate with a complex
// rotation before computing power so the slope-detection is symmetric and
// maximally sensitive.
//
// The raw power envelope has strong ~107 kHz ripple (37.5 samples period)
// caused by FM sidebands interacting with the IF filter.  A 32-sample MA
// filter attenuates 107 kHz by ~16 dB while passing the 15.625 kHz PAL
// line frequency at ~95% amplitude, and only broadens the 19-sample H-sync
// by ~16 samples (vs ~32 for MA_LEN=64), improving edge timing accuracy.

static const int    MA_LEN          = 32;
static float        maBuf[MA_LEN]   = {};
static int          maIdx           = 0;
static float        maSum           = 0.0f;
static float        corrPhase       = 0.0f;
// +1.5 MHz shift to move carrier from -1.5 MHz to DC (radians per sample)
static const float  CORR_STEP       = 2.0f * static_cast<float>(M_PI) * 1.5e6f / 4.0e6f;

static void receivedHifSamples(CaribouLiteRadio* radio,
                                const std::complex<float>* samples,
                                CaribouLiteMeta* /*sync*/,
                                size_t num_samples)
{
    std::vector<float> envelope(num_samples);
    float rawMin = 1e9f, rawMax = -1e9f;
    float filtMin = 1e9f, filtMax = -1e9f;
    float avgPower = 0.0f;

    for (size_t i = 0; i < num_samples; i++)
    {
        // 1. Frequency-correct: rotate IQ by +1.5 MHz to centre the carrier
        std::complex<float> corrected =
            samples[i] * std::complex<float>(std::cos(corrPhase), std::sin(corrPhase));
        corrPhase += CORR_STEP;
        if (corrPhase > static_cast<float>(M_PI)) corrPhase -= 2.0f * static_cast<float>(M_PI);

        // 2. Instantaneous power (I^2 + Q^2)
        float p = std::norm(corrected);
        if (p < rawMin) rawMin = p;
        if (p > rawMax) rawMax = p;
        avgPower += p;

        // 3. 64-sample moving-average LPF (circular buffer, persistent across calls)
        maSum -= maBuf[maIdx];
        maBuf[maIdx] = p;
        maSum += p;
        maIdx = (maIdx + 1) % MA_LEN;
        float filtered = maSum / MA_LEN;

        envelope[i] = filtered;
        if (filtered < filtMin) filtMin = filtered;
        if (filtered > filtMax) filtMax = filtered;
    }
    avgPower /= num_samples;

    static size_t hifCallCount = 0;
    if (++hifCallCount % 200 == 0)
    {
        float modDepth = (filtMax > 1e-15f) ? (filtMax - filtMin) / filtMax * 100.0f : 0.0f;
        std::cout << "[HiF #" << hifCallCount << "]"
                  << " freq=" << radio->GetFrequency() / 1000000.0f << "MHz"
                  << " samples=" << num_samples
                  << " avg_pwr=" << 10.0f * std::log10(avgPower + 1e-12f) << "dBfs"
                  << " filt_env=[" << filtMin << ", " << filtMax << "]"
                  << " mod_depth=" << modDepth << "%"
                  << " cb=" << (videoCallback ? "set" : "NULL")
                  << std::endl;
    }

    if (videoCallback)
        videoCallback(envelope.data(), static_cast<int>(num_samples));
}

void _startHifFpv(double freq, ReceiveVideoSamples callback)
{
    CaribouLiteRadio *hif = cl.GetRadioChannel(CaribouLiteRadio::RadioType::HiF);
    videoCallback = callback;
    // Reset MA filter and frequency-correction state on retune
    std::fill(maBuf, maBuf + MA_LEN, 0.0f);
    maIdx   = 0;
    maSum   = 0.0f;
    corrPhase = 0.0f;

    try { hif->SetFrequency((double)freq); }
    catch (...) { std::cerr << "HiF: unsupported frequency " << freq << std::endl; return; }

    hif->SetRxGain(48); // back off from max to avoid ADC saturation with power envelope
    hif->SetAgc(false); // AGC would flatten the amplitude variations we're trying to detect
    hif->StartReceiving(receivedHifSamples);
}

void _stopHifFpv()
{
    CaribouLiteRadio *hif = cl.GetRadioChannel(CaribouLiteRadio::RadioType::HiF);
    hif->StopReceiving();
    videoCallback = nullptr;
}

void _setHifFreq(double freq)
{
    CaribouLiteRadio *hif = cl.GetRadioChannel(CaribouLiteRadio::RadioType::HiF);
    try { hif->SetFrequency(freq); }
    catch (...) { std::cerr << "HiF: unsupported frequency " << freq << std::endl; }
}

extern "C" {
    void info() {
        _info();
    }

    void setFreq(int freq) {
        _setFreq(freq);
    }

    void setRxGain(int gain) {
        _setRxGain(gain);
    }

    void setAgc(bool agc) {
        _setAgc(agc);
    }

    void readRssi(ReceiveData callback) {
        _readRssi(callback);
    }

    void stopRssi() {
        _stopRssi();
    }

    void startHifFpv(double freq, ReceiveVideoSamples callback) { _startHifFpv(freq, callback); }
    void stopHifFpv()                                          { _stopHifFpv(); }
    void setHifFreq(double freq)                               { _setHifFreq(freq); }
    void setHifGain(int gain) {
        CaribouLiteRadio *hif = cl.GetRadioChannel(CaribouLiteRadio::RadioType::HiF);
        hif->SetRxGain(gain);
    }
    void setHifBandwidth(double bw_hz) {
        CaribouLiteRadio *hif = cl.GetRadioChannel(CaribouLiteRadio::RadioType::HiF);
        hif->SetRxBandwidth((float)bw_hz);
    }
    void setHifAgc(bool agc) {
        CaribouLiteRadio *hif = cl.GetRadioChannel(CaribouLiteRadio::RadioType::HiF);
        hif->SetAgc(agc);
    }
    void setHifSampleRate(double sr_hz) {
        CaribouLiteRadio *hif = cl.GetRadioChannel(CaribouLiteRadio::RadioType::HiF);
        hif->SetRxSampleRate((float)sr_hz);
    }
}