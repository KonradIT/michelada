#ifndef CARIBOUBRIDGE_H
#define CARIBOUBRIDGE_H

#include <stdbool.h>

// S1G Radio (sub-1 GHz) — RSSI scanning
void info();
void setFreq(int freq);
void setRxGain(int gain);
void setAgc(bool agc);
typedef void (*ReceiveData)(float, float);
void readRssi(ReceiveData);
void stopRssi();
void receiveDataGateway(float rssi, float freq);

// HiF Radio (1-6 GHz) — 5.8 GHz FPV PAL video decode
// freq is double (Hz): 5.8 GHz overflows int32; double holds it exactly
void setHifFreq(double freq);
typedef void (*ReceiveVideoSamples)(float*, int);
void startHifFpv(double freq, ReceiveVideoSamples callback);
void stopHifFpv();
void setHifGain(int gain);
void setHifAgc(bool agc);
void setHifSampleRate(double sr_hz);
// bw_hz: IF filter bandwidth in Hz — valid range 160000-2000000 (AT86RF215 steps:
//   160k, 200k, 250k, 320k, 400k, 500k, 630k, 800k, 1000k, 1250k, 1600k, 2000k)
// SDK rounds to nearest valid step.  Narrower BW sharpens slope-detection contrast.
void setHifBandwidth(double bw_hz);
void receiveVideoGateway(float* samples, int num_samples);

#endif
