# swaks --to test@example.com --from me@example.com --server localhost


swaks \
  --to test@ngm.dev \
  --from me@ngm.dev \
  --server localhost \
  --header "Subject: Test email from Swaks" \
  --body "Hello,\n\nThis is a test email sent via Swaks.\n\nRegards,\nMe"
