# google-issue-derived

Hand-written reconstructions of `purchases.subscriptionsv2.get` responses, not copied from any
repository. Shapes follow the Google reference
(https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptionsv2) and the
real-world reports below. Product ids, tokens and dates are invented.

| File | Based on |
|---|---|
| sub_active_obfuscated_account_only.json | response quoted in https://github.com/imdhemy/google-play-billing/issues/228 (only `obfuscatedExternalAccountId` present) |
| sub_active_profile_id_only.json | same issue: `externalAccountId` and `obfuscatedExternalProfileId` are optional, so the reverse case (profile id only) is valid |
| sub_deferred_replacement_two_line_items.json | https://github.com/imdhemy/google-play-billing/issues/304: deferred replacement returns two line items, the new one without `expiryTime` |
| sub_active_no_expiry_anywhere.json | extreme form of the same issue: no line item has `expiryTime` |
| sub_multi_line_item_addon.json | multi-line-item (add-on) subscriptions; Google RTDN reference, `lineItems` is an array |

License: no third-party text; dedicated to the project under its own license.
