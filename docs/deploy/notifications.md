# Notifications

Talaria tells you on your phone or in your browser when the agent needs you or is done, even when no hub page is open. A notice goes to your linked [Telegram](telegram.md) chat and to each browser where you turned on notifications (Web Push).

## What notifies

| Event | Default | When |
| ----- | ------- | ---- |
| Approvals | On | The agent asks before it runs something |
| Questions | On | The agent asks you to choose or answer |
| Failures | On | A turn ends with an error, or a scheduled job fails |
| Long tasks done | Off | A reply that ran for 2 minutes or more is ready |
| Scheduled jobs | Off | A scheduled job finished |

Each person picks the events, the channels and quiet hours in Settings, Notifications.

![Settings, Notifications: the events to notify about, the channels (Telegram and browser notifications), and a Turn on for this device button](images/settings-notifications.png)
 During quiet hours nothing is sent. An approval or question that still waits after 5 minutes gets one reminder. "Send a test" sends one test notice to each channel.

You get no notice for a chat you have open on a visible page, and none on Telegram for a chat that is already attached to a Telegram chat, since that chat shows the prompt. At most 20 notices go to one person in 10 minutes.

## Web Push

The install files make the Web Push key for you on the first start, so this works as soon as the conditions below hold.

1. Open the hub at an **HTTPS** address, or at `localhost`. Browsers do not offer push on plain `http://<LAN address>`.
2. Open Settings, Notifications, and choose "Turn on for this device".
3. Allow notifications when the browser asks.
4. Press "Send a test".

By device:

- **Android:** Chrome and Firefox show notifications from the browser or from the installed app.
- **iPhone and iPad:** Safari shows Web Push only for a web app on the Home Screen, on iOS 16.4 or later. Open the hub in Safari, tap Share, then Add to Home Screen, open it from there, and turn notifications on in Settings.
- **Desktop browsers** work too.

A push message carries only the kind of event, a fixed title such as "Hermes needs your approval", the backend's name and the address of the chat. It never carries the chat's title, a command or a question.

The server needs outbound HTTPS to the browsers' push services: `fcm.googleapis.com`, `updates.push.services.mozilla.com`, `web.push.apple.com`, `*.push.apple.com` and `*.notify.windows.com`. If you limit what the server can reach, allow those.

Do not change the Web Push key after people subscribed: a new key ends every browser's subscription. It is kept with the other keys (the `talaria_secrets` volume in compose, the Secret `<release>-keys` in Helm), and your backups should include it.

## Telegram notices

With Telegram linked, notices arrive in your chat with the bot, with buttons for approvals and an "Open in the hub" link. With topics turned on, a chat's approvals show in its topic and other notices in the General topic.

## It does not work

- No "Turn on for this device" button: the hub has no Web Push key, or the page is not on HTTPS or `localhost`. Check [Troubleshooting](troubleshooting.md#notifications).
- A test arrives on Telegram but not in the browser: the browser blocked notifications for the site. Allow them in the site settings, then turn it on again.
