#!/usr/bin/env bash
# Builds a debug-signed APK in build/<abi>/mandate.apk, inside the app devShell.
# Usage: apk.sh [x86_64|arm64-v8a]
set -euo pipefail
abi=${1:-arm64-v8a}
case $abi in
x86_64) triple=x86_64-linux-android ;;
arm64-v8a) triple=aarch64-linux-android ;;
*) echo "unknown abi $abi" >&2 && exit 2 ;;
esac
cd "$(dirname "$0")"
tools=$ANDROID_HOME/build-tools/35.0.0
android=$ANDROID_HOME/platforms/android-35/android.jar
out=build/$abi
rm -rf "$out" && mkdir -p "$out/lib/$abi" "$out/classes" "$out/gen"

# The crate stays an rlib on desktop; only Android needs the cdylib.
cargo ndk -t "$abi" -P 30 rustc -p mandate-app --lib --release --crate-type cdylib
cp "../../target/$triple/release/libmandate_app.so" "$out/lib/$abi/"

# Linking the resources also writes R.java, which the Java side uses.
"$tools/aapt2" compile --dir res -o "$out/res.zip"
"$tools/aapt2" link -o "$out/unaligned.apk" --manifest AndroidManifest.xml -I "$android" \
	--java "$out/gen" "$out/res.zip"
mapfile -t sources < <(find java "$out/gen" -name '*.java')
javac --release 11 -classpath "$android" -d "$out/classes" "${sources[@]}"
mapfile -t classes < <(find "$out/classes" -name '*.class')
"$tools/d8" --release --min-api 30 --lib "$android" --output "$out" "${classes[@]}"
(cd "$out" && zip -qr unaligned.apk lib classes.dex)
"$tools/zipalign" -f -P 16 4 "$out/unaligned.apk" "$out/mandate.apk"
[ -f build/debug.keystore ] || keytool -genkeypair -keystore build/debug.keystore \
	-storepass android -alias debug -keyalg RSA -validity 10000 -dname CN=debug
"$tools/apksigner" sign --ks build/debug.keystore --ks-pass pass:android "$out/mandate.apk"
