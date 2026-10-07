# Alerts

Alerts watch your numbers and notify you: in the app (the bell), in your browser (Web Push, also on a phone with the
UI added to the home screen) and through a webhook (Slack, Mattermost, Discord or anything that accepts JSON). No
mail server, no external push service account: the server generates its Web Push (VAPID) keys on first start.

## Setup in two minutes

1. **Alerts → Enable on this device.** The browser asks for permission. Repeat on every device that should be
   notified (each browser is one subscription). **Send test** checks delivery.
2. **New alert:** pick an example or write a condition, set "for" and "cooldown", tick browser notification and/or
   paste a webhook URL. The editor shows whether the condition would alert right now.
3. **Test** in the alert list sends a test notification through the alert's channels.

Presets (website, shop, saas) create sensible alerts for you; they only start alerting after the first data
arrived.

Browser notifications need HTTPS (or `localhost`). Behind a reverse proxy set `AGG_PUBLIC_URL` so links in
notifications point to the right address.

## Conditions

A condition is a boolean expression over [variables](aggregates.md#variables), formulas and `now` (unix seconds):

```
orders_1h < 1                                              no orders in the last hour
now - last_purchase > 3 * 3600                             no purchase for 3 hours (last_timestamp aggregate)
revenue_prev_24h > 0 && revenue_24h < 0.5 * revenue_prev_24h   revenue down 50% vs the day before
page_views_total > 0 && page_views_6h < 1                  traffic stopped (only after the site ever had traffic)
product_viewers_5m > 50                                    with dims {"product": "73"}: one product is suddenly hot
aov < 50                                                   a formula
```

`dims` narrows grouped aggregates to one value, like `?product=73` in the values API.

If a value is missing (e.g. no purchase yet for `last_purchase`), the alert is not evaluated and shows
"no value for …"; its state does not change.

## States

Every minute each enabled alert is evaluated:

- **ok** → condition true → **pending**;
- **pending** for at least *for* minutes → **firing**, and a notification is sent (unless outside the schedule or
  inside the cooldown);
- **pending** → condition false → back to **ok**, silently (a short dip does not notify);
- **firing** → condition false → **ok**; with "notify when resolved" a resolved notification is sent.

**Cooldown** is the minimum time between two firing notifications of the same alert, so a flapping condition does
not spam. **Schedule** (days, hours, timezone) limits when notifications are sent; outside it the state still changes,
silently. History (Alerts → History) keeps every state change for 90 days.

## Webhook payload

```json
{
  "status": "firing",
  "alert": "no_orders",
  "title": "Alert: No orders for 3 hours",
  "site": "shop",
  "condition": "now - last_purchase > 3 * 3600",
  "values": "last_purchase = 1791380000",
  "url": "https://agg.example.com/#/s/1/alerts",
  "time": "2026-10-07T12:00:00Z",
  "text": "Alert: No orders for 3 hours: now - last_purchase > 3 * 3600 · last_purchase = 1791380000 · Shop",
  "content": "…same as text…"
}
```

`status` is `firing`, `resolved` or `test`. `text` is what Slack and Mattermost show, `content` is what Discord
shows.

## Prometheus users

If you already run Prometheus and Alertmanager you can alert there instead, on the export metrics, e.g.
`time() - agg_last_timestamp_seconds{aggregate="last_purchase"} > 10800`.
