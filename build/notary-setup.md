# Одноразове налаштування нотарізації

```bash
xcrun notarytool store-credentials tg-archive-notary \
  --apple-id "твій@apple.id" \
  --team-id "TEAMID" \
  --password "app-specific-password" \
  --no-validate
```

`app-specific-password` створюється на https://appleid.apple.com → Sign-In and Security →
App-Specific Passwords. Це **не** пароль від Apple ID, і Apple показує його рівно один раз.

Перевірити: `xcrun notarytool history --keychain-profile tg-archive-notary`

## Чому `--no-validate`

Вбудована перевірка в `store-credentials` ходить не тим ендпоінтом, що робочий API, і вміє
віддавати `HTTP status code: 401. Invalid credentials` на цілком валідний пароль — після чого
профіль не зберігається взагалі. Якщо прямий виклик із тими самими даними працює:

```bash
xcrun notarytool history --apple-id "твій@apple.id" --team-id "TEAMID" --password "asp"
```

то справа саме в перевірці, а не в паролі. Окремо переконатись у паролі можна через
`xcrun altool --list-providers -u "твій@apple.id" -p "asp"` — altool відповідає розгорнуто,
а не голим 401.

## Якщо профіль дає 401, а явні аргументи працюють

У кейчейні лишився недоформований запис від невдалої спроби. Видали профіль і збережи заново.
Запис живе в «Local Items», тому `security find-generic-password -s com.apple.gke.notary.tool`
його **не бачить** — його відсутність у видачі ще не означає, що профілю немає.

## Ознака, що все спрацювало

```
spctl -a -vvv -t install dist/tg-archive
```

`source=Notarized Developer ID` — добре. `source=Unnotarized Developer ID` означає, що бінарник
підписаний, але нотарізація не відпрацювала, і Gatekeeper його не пропустить.
