# ☁️ LTS Cloud

LTS Cloud is a small relay server for the [LTS Respooler](https://github.com/LukasT03/LTS-Respooler). It lets LTS Control reach your board when you're not at home, so you can check the progress and pause or stop a respool from anywhere. At home, the app still talks to the board directly over WiFi or Bluetooth.

It's written in Go with SQLite and runs at `cloud.lts-design.com`. Remote access can be turned off in the app, then the board never connects to it.

## 🔒 Privacy

The server only passes messages between your board and your app. For each board it stores a random ID, a hash of its secret, the last status (the same values the app shows) and when it was last online. No accounts, no names, no IP addresses. Boards that haven't been online for 180 days are deleted.

Only devices that got the board's ID and secret over Bluetooth or your local network can see or control it.

## 📋 License

This project is licensed under the Mozilla Public License 2.0 (MPL 2.0). You’re free to use, modify, and share the code in your own projects. If you make changes and distribute them, just keep those modified files open under the same license.

Contributions, modifications, and improvements are very welcome! :)
