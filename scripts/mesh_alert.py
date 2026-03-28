#!/usr/bin/env python3
import argparse
import sys
import time
import meshtastic.serial_interface

TARGET_SHORT_NAME = "r111"

DEBUG = True

def find_node(iface, name):
    """Find a node's numeric ID by shortName or longName."""
    if not iface.nodes:
        return None
    for node in iface.nodes.values():
        user = node.get("user", {})
        if user.get("shortName") == name or user.get("longName") == name:
            return node["num"]
    return None


def main():
    parser = argparse.ArgumentParser(description="Send FPV detection alert via Meshtastic")
    parser.add_argument("--freq", required=True, type=int, help="Detected frequency in Hz")
    args = parser.parse_args()

    freq_mhz = args.freq / 1_000_000
    msg = f"Detected: {freq_mhz:.0f} MHz"

    if DEBUG:
        print(f"Connecting to Meshtastic device...", flush=True)
    try:
        iface = meshtastic.serial_interface.SerialInterface(devPath="") # no port specified, have the library auto-detect the port
    except Exception as e:
        print(f"Failed to connect: {e}", file=sys.stderr)
        sys.exit(1)

    try:
        target_id = find_node(iface, TARGET_SHORT_NAME)
        if target_id is None:
            sys.exit(1)
        if DEBUG:
            print(f"Sending to {TARGET_SHORT_NAME} (id={target_id}): {msg}", flush=True)
        iface.sendText(msg, destinationId=target_id, wantAck=True)

        # Brief pause to let the radio transmit
        time.sleep(5)
        if DEBUG:
            print("Done.", flush=True)
    finally:
        iface.close()


if __name__ == "__main__":
    main()
