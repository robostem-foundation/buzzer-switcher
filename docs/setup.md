# Set up FieldCam

## Connect to OBS

1. Extract the Windows ZIP and open **FieldCam Buzzer Switcher.exe**. No installer is needed. The EXE is unsigned.
2. In OBS, open **Tools → WebSocket Server Settings**. Enable the server and keep authentication on.
3. Enter the address and password in FieldCam. Use `127.0.0.1:4455` if OBS is on the same PC and uses the default port.
4. Click **Load OBS Scenes**. Add the field scenes in the order matches will be played. FieldCam loops back to the first scene after the last.

## Test before going live

1. Before the start buzzer, click **Listen Only**. Run a full practice match and look for **Start sound matched** and **End sound matched** in the log. This mode never changes scenes.
2. With the stream off-air, put a field scene live in OBS and click **Arm Switching** before the next start buzzer. FieldCam should switch to the next field with the transition already selected in OBS.
3. Click **Stop** to cancel a pending switch. Stop during Skills, breaks, or when the field order changes. FieldCam can't tell matches apart if they use the same sounds.

| Setting | Default |
| --- | --- |
| Require start buzzer | On |
| Time from start to end | 45–70 seconds |
| Switch delay from the start of the end buzzer | 2 seconds |
| Match threshold | 88% |

If you arm midway through a match, FieldCam waits for the next start buzzer. It skips the switch if your live scene is not in the scene list, or if a different scene is live when the delay ends. Hover over an info icon for help with a setting.

## If the meter stays flat

Check where Tournament Manager sends its sound. FieldCam listens to the default Windows playback device on the PC running FieldCam, not the OBS mixer or a microphone.

FieldCam uses installed `Start.wav` and `Stop.wav` files when available. Otherwise it uses built-in fingerprints. You can also load a custom end WAV. The app processes audio on your PC without recording it. It saves your scene order and timing on exit, but not the OBS password or armed state.
