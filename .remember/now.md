
## 11:23 | feat/fleet-login-code
Sec review of fleet_say.go, send_keys.go, send_keys_test.go: moved tmuxPaneInMode into send_keys.go, refused sendKeys to copy-mode panes, added "--" before text, added sendKeysPaneInMode/sendKeysTmux seams and 3 tests.