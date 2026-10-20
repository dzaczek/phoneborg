# Demo GIF for the README: `vhs assets/demo/demo.tape` from the repo root.
# Needs a controller on 127.0.0.1:18080 (or an ssh tunnel to one), pbctl in
# $PB_BIN and PHONEBORG_ADMIN_TOKEN set. The hidden setup trims the pbctl
# tables to fit the frame and blanks phone serial numbers; output is live.
Output assets/demo.gif

Set Shell bash
Set FontSize 15
Set Width 1200
Set Height 640
Set Padding 24
Set Theme "GitHub Dark"
Set TypingSpeed 35ms

Hide
Type "export PS1='$ ' PATH=$PB_BIN:$PATH" Enter
Type `mask() { perl -pe 's/ \(([0-9A-Za-z]+)\)/" " x (length($1)+3)/ge; s/\b[0-9a-f]{8}\b/********/g'; }` Enter
Type `pbctl() { case $1 in nodes) command pbctl nodes | mask | cut -c1-23,39-46,54-112,145-150;; pools) command pbctl pools | mask | awk 'NF==0{exit}1' | cut -c1-78;; *) command pbctl "$@";; esac; }` Enter
Type `stream() { sed -un '/DONE/d; s/^data: //p' | jq -rj --unbuffered '.choices[0].delta.content? // empty'; echo; }` Enter
Type "clear" Enter
Show

Type "# five old phones, one cluster" Enter
Sleep 500ms
Type "pbctl nodes" Enter
Sleep 4s

Type "# one OpenAI-compatible endpoint; the gateway picks a phone serving the model" Enter
Type `curl -sN 127.0.0.1:18080/v1/chat/completions -d '{"model":"qwen3-4b-instruct-2507-q4_k_m","stream":true,"messages":[{"role":"user","content":"In two sentences: why run AI on old phones?"}]}' | stream`
Enter
Sleep 14s

Type "# pools, Super Borg and Big Brain on the same phones" Enter
Type "pbctl pools" Enter
Sleep 6s
