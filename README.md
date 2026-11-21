<h1 align="center">FieldCam Buzzer Switcher</h1>
<p align="center">Switch OBS field cameras when a VEX IQ match ends.</p>
<p align="center">
  <strong>Windows 10/11 (x64)</strong> · <strong>OBS Studio 28+</strong><br>
  <a href="https://github.com/robostem-foundation/buzzer-switcher/releases">Windows releases</a> · <a href="docs/setup.md">Setup guide</a> · <a href="docs/release-notes.md">Release notes</a>
</p>
<p align="center">
  <img src="docs/images/fieldcam-preview.png" width="600" alt="FieldCam window preview with OBS connection, scene order, buzzer settings, and Listen Only, Arm Switching, and Stop buttons"><br>
  <sub>Layout preview based on the Windows app.</sub>
</p>

## Get started

1. Extract the Windows ZIP and open **FieldCam Buzzer Switcher.exe**. No installer is needed.
2. In OBS, go to **Tools → WebSocket Server Settings** and enable the server. In FieldCam, use `127.0.0.1:4455` if OBS is on the same PC and uses the default port. Enter the OBS WebSocket password.
3. Click **Load OBS Scenes** and put your field scenes in match order. Run a full practice match in **Listen Only**. Look for **Start sound matched** and **End sound matched** before you try **Arm Switching** off-air.

## What triggers a switch?

FieldCam advances to the next scene in your list and wraps back to the first. By default, it accepts an end buzzer only **45–70 seconds after the start buzzer**. The switch starts **two seconds after the end buzzer begins** and uses the transition already selected in OBS. Both timings are adjustable.

Click **Stop** during Skills, breaks, or when your fields are out of order. FieldCam can't tell fields or match types apart if they use the same sounds. See the [setup guide](docs/setup.md) for the full test and default settings.

## Audio and settings

FieldCam listens to the default Windows playback device, not the OBS mixer or a microphone. It processes audio on your PC without recording it. It uses installed Tournament Manager buzzer sounds when available, or built-in fingerprints. No TM API key is needed.

FieldCam keeps one WebSocket connection open to OBS. It saves your scene order and timing, but never saves the OBS password or arms itself on startup.

## Build from source

Use Go 1.24 or later. From the repository root in PowerShell:

```powershell
go test ./...
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
$env:CGO_ENABLED = '0'
go build -trimpath -ldflags="-H=windowsgui -s -w" -o "FieldCam Buzzer Switcher.exe" ./app
```

An independent RoboSTEM Foundation project. Not affiliated with VEX Robotics or OBS Studio.
