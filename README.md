# Michelada - Electronic Warfare tool

Use your CaribouLite SDR as a field-portable spectrum analyzer, FPV drone detector, REB (jamming/spoofing devices) checker and more.

**Look how damn tiny it is!**

![Michelada](./images/cariboulite.png)

## Features

- Uses the CaribouLite SDR radios to scan 100 Mhz - 6 GHz
- Spectrum analyzer with RBW control, labels
- FPV drone detector using pseudo-complete FM demodulation with PAL/NTSC, as well as color subcarrier embedded in the video signal detection, it also filters out jamming and other RF noise to only detect the video signal, and show an alert on the browser when an FPV drone feed is detected
- Check your REB effectiveness with easy to understand UI
- Runs on the Pi, just install and have `./michelada` run automatically on boot, then navigate to pi IP address in your browser: http://pi-ip-address:8080
- Can be used via USB EThernet using the Pi's USB port, or via WiFi if you provide a hotspot

## Modes

1. Spectrum analyzer

![](./images/spectrum-analyzer-view.png)

Also works on mobile:

<img src="./images/spectrum-analyzer-view-mobile.png" alt="Spectrum analyzer view on mobile" width="300">

2. FPV video analyzer

![](./images/fpv-video.png)

3. REB checker

![](./images/reb-check.png)

4. Detector

Runs standalone without a browser, set in michelada.json:

```json
{
  "detector_start_on_boot": true,
  "detector_bands": ["5_8", "1_2"],
  "detector_cooldown": 60
}
```

Can be extended with scripts to run when a drone is detected, set in michelada.json:

```json
{
  "scripts": {
    "on_video_detected": [
      "python3 /home/pi/samples/scripts/mesh_alert.py",
    ]
  }
}
```

![](./images/scripting-demo.png)

## Config:

First before doing anything, you need to configure the device. This is done by editing the michelada.json file.

Location: `/home/pi/michelada.json`

Copy the michelada.sample.json to michelada.json, disable/enable the modes you want to use.

- `detector_start_on_boot`: Whether to start the detector on boot, this won't allow you to use the other modes.
- `detector_bands`: The bands to scan for drones, can be `5_8`, `1_2`, `3_3`.
- `detector_cooldown`: The cooldown time in seconds between detections.
- `default_spectrum_frequencies`: The default frequencies to scan for the spectrum analyzer, set in MHz.
- `scripts`: The scripts to run when a drone is detected in Detector mode, set in the `on_video_detected` array.

## Installation

First, setup your Raspberry Pi with a fresh install of Raspberry Pi OS Bookworm Lite.

Personally I use:

```
$ uname -a
Linux drone 6.12.47+rpt-rpi-v8 #1 SMP PREEMPT Debian 1:6.12.47-1+rpt1~bookworm (2025-09-16) aarch64 GNU/Linux
```

Install cariboulite SDK, drivers and stuff according to their instructions: https://github.com/cariboulabs/cariboulite

Feel free to follow Jeff Geerling's guide: https://www.jeffgeerling.com/blog/2025/cariboulite-sdr-hat-sdr-on-raspberry-pi/

because installing the cariboutlite sdk is as enjoyable as pulling your fingernails off.

Now install Go 1.24:

```bash
GOVERSION="1.24.4"
wget "https://golang.org/dl/go${GOVERSION}.linux-arm64.tar.gz" -4
tar -C /usr/local -xvf "go${GOVERSION}.linux-arm64.tar.gz"
rm "go${GOVERSION}.linux-arm64.tar.gz"
```

Add Go to your PATH:

```bash
cat >> ~/.bashrc << 'EOF'
export GOPATH=$HOME/go
export PATH=/usr/local/go/bin:$PATH:$GOPATH/bin
EOF

source ~/.bashrc
```

Now clone the repository:

```bash
git clone https://github.com/konradit/michelada.git
cd michelada
```

And finally:

runner.sh will compile the software, it will take a lot of time, grab a michelada while you wait.
```bash
./runner.sh

ls michelada
```

## Why the name?

[IYKYK](https://en.wikipedia.org/wiki/Shibboleth), it's a tasty drink, [try it.](https://www.chilipeppermadness.com/chili-pepper-recipes/drinks/michelada-recipe-traditional/)

## Shouts

- [Jeff Geerling](https://www.jeffgeerling.com/) for his guide on installing the cariboulite SDK
- d0tslash for general guidance