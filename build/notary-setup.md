# Одноразове налаштування нотарізації

```bash
xcrun notarytool store-credentials tg-archive-notary \
  --apple-id "твій@apple.id" \
  --team-id 4JV3A5SUSZ \
  --password "app-specific-password"
```

`app-specific-password` створюється на https://appleid.apple.com → Sign-In and Security →
App-Specific Passwords. Це **не** пароль від Apple ID.

Перевірити: `xcrun notarytool history --keychain-profile tg-archive-notary`
