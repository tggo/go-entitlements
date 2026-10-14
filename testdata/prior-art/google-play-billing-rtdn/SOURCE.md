# google-play-billing-rtdn

- Repo: https://github.com/imdhemy/google-play-billing
- Commit: 680110f31de0b88cf74d2a7b9fea423ade08e6c3
- License: MIT (Copyright (c) Mohamad Eldhemy)
- Origin: `tests/AAA/RtdnProvider.php` (PHP arrays), converted to JSON. `*.push.json` wraps the same
  payload in a Pub/Sub push envelope (base64 `message.data`, `messageId` 136969346945) the way
  `cloudMessage()` does there.

| File | What |
|---|---|
| test.json / test.push.json | testNotification |
| voided-subscription.json / .push.json | voidedPurchaseNotification, productType 1 |
| voided-one-time-DERIVED.json / .push.json | DERIVED by us: same as above with productType 2 (one-time product); not in the source repo |
| one-time-product.json / .push.json | oneTimeProductNotification (sku, notificationType 1) |
| subscription-no-subscriptionId.json / .push.json | subscriptionNotification type 4 without `subscriptionId` (see imdhemy/google-play-billing#274, laravel-in-app-purchases#437) |

Values are invented test data (package com.some.app, token PURCHASE_TOKEN).
