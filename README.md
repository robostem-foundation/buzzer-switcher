# FieldCam Buzzer Switcher

Switch OBS field cameras when a VEX IQ match ends. FieldCam listens for the match buzzer on Windows and advances through the scenes you choose. It doesn't connect to Tournament Manager or need a TM API key.

[Windows releases](https://github.com/robostem-foundation/buzzer-switcher/releases) · [Setup guide](docs/setup.md) · [Release notes](docs/release-notes.md)

**Requires Windows 10 or 11 (x64) and OBS Studio 28 or later.**

## Get started

1. Open **FieldCam Buzzer Switcher.exe**. In OBS, enable the WebSocket server under **Tools → WebSocket Server Settings**.
2. In FieldCam, load your OBS scenes and put the field cameras in match order.
3. Run a full practice match in **Listen Only**. Check for **Start sound matched** and **End sound matched** before trying **Arm Switching** off-air.

By default, the end buzzer must come 45–70 seconds after the start. FieldCam then waits until two seconds after the end buzzer began to switch scenes. OBS uses the transition you've selected.

<details>
<summary>See the Windows layout</summary>

<img src="docs/images/fieldcam-preview.png" width="440" alt="FieldCam layout preview showing OBS connection, scene order, timing controls, and Listen Only">

</details>

## Build from source

Use Go 1.24 or later. From the repository root in PowerShell:

```powershell
go test ./...
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
$env:CGO_ENABLED = '0'
go build -trimpath -ldflags="-H=windowsgui -s -w" -o "FieldCam Buzzer Switcher.exe" ./app
```

[App code and tests](app/) · [Buzzer fingerprints](app/cues/)

A RoboSTEM Foundation project. Not affiliated with VEX Robotics or OBS Studio.
