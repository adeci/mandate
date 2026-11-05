//! Android: the entry point, the Keystore device key, the links and
//! notifications that open the app, and the native half of the stream
//! service. The Java half is in android/java.

use crate::device::{Device, FILE, hex, unhex};
use crate::fido::android::Fido;
use futures::StreamExt;
use futures::channel::mpsc::{UnboundedReceiver, UnboundedSender, unbounded};
use gpui::WindowOptions;
use gpui_mobile::android::{jni as gpui_jni, window::WindowAppearance};
use jni::errors::{LogErrorAndDefault, ThrowRuntimeExAndDefault};
use jni::objects::{JByteArray, JClass, JObject, JString, JValue};
use jni::refs::Global;
use jni::signature::MethodSignature;
use jni::strings::JNIStr;
use jni::{Env, EnvUnowned, JavaVM, jni_sig, jni_str};
use mandate_core::{Client, DeviceKey, DeviceSignature, Error, der_signature};
use serde_json::{Value, json};
use std::collections::HashSet;
use std::ffi::c_void;
use std::panic::AssertUnwindSafe;
use std::path::{Path, PathBuf};
use std::sync::atomic::Ordering::Relaxed;
use std::sync::atomic::{AtomicBool, AtomicU32};
use std::sync::{Arc, LazyLock, Mutex, OnceLock};
use std::time::Duration;

#[unsafe(no_mangle)]
fn android_main(app: android_activity::AndroidApp) {
    gpui_jni::install_panic_hook();
    let _ = std::panic::catch_unwind(AssertUnwindSafe(|| run(app)));
    // The activity is gone, or the app panicked. gpui-mobile's platform
    // belongs to the process, so a second android_main here would panic:
    // leave, and Android starts a new process for the next activity.
    std::process::exit(0);
}

fn run(app: android_activity::AndroidApp) {
    let dir = app.internal_data_path().expect("an app data dir");
    gpui_jni::init_platform(&app);
    let platform = gpui_jni::shared_platform().expect("platform initialised");
    if dir.join(FILE).exists() {
        listen();
    }
    let mut opened = OPENED.1.lock().unwrap().take().expect("one android_main per process");
    let app = gpui::Application::with_platform(platform.into_rc()).with_assets(crate::Assets);
    app.run(move |cx| {
        let shell = crate::open(cx, WindowOptions::default(), dir);
        // gpui-mobile starts windows light and only follows later changes.
        // The window is fetched here: tasks run under gpui-mobile's platform
        // lock, which primary_window takes too.
        if let Some(w) = gpui_jni::shared_platform().and_then(|p| p.0.primary_window()) {
            let dark = gpui_jni::query_night_mode_via_jni();
            let appearance = if dark { WindowAppearance::Dark } else { WindowAppearance::Light };
            cx.defer(move |_| w.set_appearance(appearance));
        }
        cx.spawn(async move |cx| {
            while let Some(o) = opened.next().await {
                shell.update(cx, |shell, cx| match o {
                    Opened::Link(link) => shell.take_link(link, cx),
                    Opened::Item(id) => shell.take_item(id, cx),
                    Opened::Redraw => cx.notify(),
                    Opened::Error(e) => {
                        shell.error = Some(e);
                        cx.notify();
                    }
                    Opened::Foreground => shell.foreground(cx),
                });
            }
        })
        .detach();
    });
}

/// What an intent or the scanner opened the app with, a change to lay out
/// again for (new insets, the clock switching between 12 and 24 hours), an
/// error to show, or the app coming back from the background.
enum Opened {
    Link(String),
    Item(String),
    Redraw,
    Error(String),
    Foreground,
}

#[unsafe(no_mangle)]
extern "system" fn Java_dev_mandate_app_Main_foreground(_: EnvUnowned, _: JClass) {
    let _ = OPENED.0.unbounded_send(Opened::Foreground);
}

/// Intents can arrive before android_main takes the receiver.
type Inbox = (UnboundedSender<Opened>, Mutex<Option<UnboundedReceiver<Opened>>>);

static OPENED: LazyLock<Inbox> = LazyLock::new(|| {
    let (tx, rx) = unbounded();
    (tx, Mutex::new(Some(rx)))
});

#[unsafe(no_mangle)]
extern "system" fn Java_dev_mandate_app_Main_opened<'a>(
    mut env: EnvUnowned<'a>,
    _: JClass<'a>,
    link: JString<'a>,
    item: JString<'a>,
) {
    env.with_env(|env| -> jni::errors::Result<()> {
        if !link.is_null() {
            let _ = OPENED.0.unbounded_send(Opened::Link(link.try_to_string(env)?));
        }
        if !item.is_null() {
            let _ = OPENED.0.unbounded_send(Opened::Item(item.try_to_string(env)?));
        }
        Ok(())
    })
    .resolve::<LogErrorAndDefault>()
}

/// The top and bottom system-bar and cutout insets, in logical px, as bits,
/// and whether the soft keyboard shows; atomics so that rendering takes no
/// lock.
static INSETS: [AtomicU32; 2] = [AtomicU32::new(0), AtomicU32::new(0)];
static KEYBOARD: AtomicBool = AtomicBool::new(false);

/// The top and bottom insets the window draws under, in logical px.
pub fn insets() -> (f32, f32) {
    let [top, bottom] = &INSETS;
    (f32::from_bits(top.load(Relaxed)), f32::from_bits(bottom.load(Relaxed)))
}

/// Whether the soft keyboard is up.
pub fn keyboard_up() -> bool {
    KEYBOARD.load(Relaxed)
}

#[unsafe(no_mangle)]
extern "system" fn Java_dev_mandate_app_Main_insets(
    _: EnvUnowned,
    _: JClass,
    top: jni::sys::jfloat,
    bottom: jni::sys::jfloat,
    keyboard: jni::sys::jboolean,
) {
    INSETS[0].store(top.to_bits(), Relaxed);
    INSETS[1].store(bottom.to_bits(), Relaxed);
    KEYBOARD.store(keyboard == jni::sys::JNI_TRUE, Relaxed);
    let _ = OPENED.0.unbounded_send(Opened::Redraw);
}

/// Whether gpui has opened the window and asked for its first frame.
#[unsafe(no_mangle)]
extern "system" fn Java_dev_mandate_app_Main_started(
    _: EnvUnowned,
    _: JClass,
) -> jni::sys::jboolean {
    gpui_jni::NATIVE_INITIALIZED.load(std::sync::atomic::Ordering::Acquire)
}

#[unsafe(no_mangle)]
extern "system" fn Java_dev_mandate_app_Main_clock(
    _: EnvUnowned,
    _: JClass,
    h24: jni::sys::jboolean,
) {
    let h24 = h24 == jni::sys::JNI_TRUE;
    if crate::words::H24.swap(h24, Relaxed) != h24 {
        let _ = OPENED.0.unbounded_send(Opened::Redraw);
    }
}

/// Shows or hides the soft keyboard. NativeActivity hands its keys to gpui
/// as KeyDownEvents, as with a hardware keyboard.
pub fn keyboard(show: bool) {
    if let Some(app) = gpui_jni::android_app() {
        if show { app.show_soft_input(true) } else { app.hide_soft_input(false) }
    }
}

/// The clipboard's text, if any.
pub fn clipboard() -> Option<String> {
    text(jni_str!("clipboard"))
}

/// The phone's name in its settings, else its model.
pub fn device_name() -> Option<String> {
    text(jni_str!("deviceName"))
}

/// Calls an activity method that answers a String or null.
fn text(method: &JNIStr) -> Option<String> {
    let text = activity_call(|env, activity| {
        let sig = jni_sig!("()Ljava/lang/String;");
        let text = env.call_method(activity, method, sig, &[])?.l()?;
        let text = env.cast_local::<JString>(text)?;
        if text.is_null() { Ok(None) } else { text.try_to_string(env).map(Some) }
    });
    text.inspect_err(|e| log::error!("{method}: {e}")).ok().flatten()
}

/// Puts text on the clipboard.
pub fn copy(text: &str) {
    let copied = activity_call(|env, activity| {
        let text = env.new_string(text)?;
        let sig = jni_sig!("(Ljava/lang/String;)V");
        env.call_method(activity, jni_str!("copy"), sig, &[(&text).into()])?.v()
    });
    if let Err(e) = copied {
        log::error!("copy: {e}");
    }
}

/// Back at the app's root: the app goes to the background, as other
/// Android apps do.
pub fn leave() {
    let left = activity_call(|env, activity| {
        env.call_method(activity, jni_str!("leave"), jni_sig!("()V"), &[])?.v()
    });
    if let Err(e) = left {
        log::error!("leave: {e}");
    }
}

/// Opens the camera app. Its QR reading opens the setup page's
/// mandate://pair link, which brings the app back on the pairing screen.
pub fn scan() {
    let opened = activity_call(|env, activity| {
        env.call_method(activity, jni_str!("scan"), jni_sig!("()V"), &[])?.v()
    });
    if let Err(e) = opened {
        log::error!("scan: {e}");
    }
}

#[unsafe(no_mangle)]
extern "system" fn Java_dev_mandate_app_Scan_frame<'a>(
    mut env: EnvUnowned<'a>,
    _: JClass<'a>,
    luma: JByteArray<'a>,
    width: jni::sys::jint,
    height: jni::sys::jint,
    stride: jni::sys::jint,
) -> jni::sys::jint {
    env.with_env(|env| -> jni::errors::Result<jni::sys::jint> {
        let luma = env.convert_byte_array(&luma)?;
        let (w, h, stride) = (width as usize, height as usize, stride as usize);
        Ok(match read_qr(&luma, w, h, stride) {
            None => 0,
            Some(link) if link.starts_with("mandate://pair?") => {
                let _ = OPENED.0.unbounded_send(Opened::Link(link));
                1
            }
            Some(_) => 2,
        })
    })
    .resolve::<LogErrorAndDefault>()
}

/// The text of a QR code in a greyscale frame, if it holds one.
fn read_qr(luma: &[u8], w: usize, h: usize, stride: usize) -> Option<String> {
    if w == 0 || h == 0 || luma.len() < (h - 1) * stride + w {
        return None;
    }
    let mut image = rqrr::PreparedImage::prepare_from_greyscale(w, h, |x, y| luma[y * stride + x]);
    image.detect_grids().into_iter().find_map(|g| g.decode().ok()).map(|(_, text)| text)
}

#[unsafe(no_mangle)]
extern "system" fn Java_dev_mandate_app_Scan_denied(_: EnvUnowned, _: JClass) {
    let no =
        "mandate may not use the camera. Paste the link instead, or allow the camera in Settings.";
    let _ = OPENED.0.unbounded_send(Opened::Error(no.into()));
}

/// Calls into the activity (Main.java).
fn activity_call<T>(
    f: impl FnOnce(&mut Env, &JObject) -> jni::errors::Result<T>,
) -> Result<T, Error> {
    java(|env, _| {
        let activity = activity(env);
        f(env, &activity)
    })
}

/// The Java classes native code calls. They are found while the library
/// loads, as only then does JNI search the app's own classes.
pub(crate) struct Classes {
    keys: Global<JClass<'static>>,
    stream: Global<JClass<'static>>,
    pub(crate) fido: Global<JClass<'static>>,
}

static CLASSES: OnceLock<Classes> = OnceLock::new();

#[unsafe(no_mangle)]
extern "system" fn JNI_OnLoad(vm: *mut jni::sys::JavaVM, _: *mut c_void) -> jni::sys::jint {
    android_logger::init_once(
        android_logger::Config::default()
            .with_max_level(log::LevelFilter::Info)
            .with_tag("mandate"),
    );
    let vm = unsafe { JavaVM::from_raw(vm) };
    let found = vm.attach_current_thread(|env| -> jni::errors::Result<Classes> {
        let keys = env.find_class(jni_str!("dev/mandate/app/Keys"))?;
        let stream = env.find_class(jni_str!("dev/mandate/app/Stream"))?;
        let fido = env.find_class(jni_str!("dev/mandate/app/Fido"))?;
        Ok(Classes {
            keys: env.new_global_ref(keys)?,
            stream: env.new_global_ref(stream)?,
            fido: env.new_global_ref(fido)?,
        })
    });
    match found {
        Ok(classes) => _ = CLASSES.set(classes),
        Err(e) => log::error!("JNI_OnLoad: {e}"),
    }
    jni::sys::JNI_VERSION_1_6
}

/// Runs f with this thread's JNI env; a Java exception becomes an Error
/// with its message.
pub(crate) fn java<T>(
    f: impl FnOnce(&mut Env, &Classes) -> jni::errors::Result<T>,
) -> Result<T, Error> {
    let classes = CLASSES.get().ok_or_else(|| Error::Key("the Java side is missing".into()))?;
    JavaVM::singleton()
        .and_then(|vm| {
            vm.attach_current_thread(|env| {
                env.with_local_frame(16, |env| {
                    let result = f(env, classes);
                    if result.is_err() {
                        env.exception_catch()?;
                    }
                    result
                })
            })
        })
        .map_err(|e| match e {
            jni::errors::Error::CaughtJavaException { msg, .. } => Error::Key(msg),
            e => Error::Key(e.to_string()),
        })
}

/// Calls a static method of Keys that answers bytes.
fn keys(
    env: &mut Env,
    classes: &Classes,
    name: &JNIStr,
    sig: MethodSignature,
    args: &[JValue],
) -> jni::errors::Result<Vec<u8>> {
    let out = env.call_static_method(&classes.keys, name, sig, args)?.l()?;
    let out = JByteArray::cast_local(env, out)?;
    env.convert_byte_array(out)
}

/// The activity, from android-activity, for what needs one.
pub(crate) fn activity<'a>(env: &Env<'a>) -> JObject<'a> {
    unsafe { JObject::from_raw(env, gpui_jni::activity_as_ptr() as jni::sys::jobject) }
}

/// How the owner approves: a strong biometric or the screen lock, or the
/// screen lock alone, both with a Keystore key; or a FIDO2 security key.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Method {
    Biometric = 1,
    ScreenLock = 2,
    SecurityKey = 3,
}

impl Method {
    /// What Settings calls it.
    pub fn title(self) -> &'static str {
        match self {
            Method::Biometric => "Fingerprint or face",
            Method::ScreenLock => "Screen lock",
            Method::SecurityKey => "Security key",
        }
    }

    /// The server's unsigned label for a key of this method.
    pub fn label(self) -> &'static str {
        match self {
            Method::Biometric => "fingerprint",
            Method::ScreenLock => "screen lock",
            Method::SecurityKey => "security key",
        }
    }
}

/// The phone's device key: in the Keystore, or on a security key.
pub enum Key {
    Keystore(Keystore),
    Fido(Fido),
}

impl Key {
    pub fn method(&self) -> Method {
        match self {
            Key::Keystore(k) => k.method(),
            Key::Fido(_) => Method::SecurityKey,
        }
    }

    /// The key as device.json keeps it: a Keystore alias, or the security
    /// key's credential.
    pub fn load(v: &Value) -> Option<Key> {
        Some(match v.get("fido2") {
            Some(f) => {
                let point = unhex(f["public_key"].as_str()?)?.try_into().ok()?;
                Key::Fido(Fido::new(point, unhex(f["credential_id"].as_str()?)?))
            }
            None => Key::Keystore(Keystore::load(v["alias"].as_str().unwrap_or(FIRST_ALIAS))?),
        })
    }

    pub fn saved(&self) -> Value {
        match self {
            Key::Keystore(k) => json!({ "alias": k.alias() }),
            Key::Fido(k) => json!({ "fido2": {
                "public_key": hex(&k.public_key()),
                "credential_id": hex(k.credential_id()),
            } }),
        }
    }

    pub fn title(&self) -> String {
        self.method().title().into()
    }

    pub fn label(&self) -> &'static str {
        self.method().label()
    }

    /// Whether signing needs the PIN first: a security key's, once per
    /// signature. The Keystore asks on its own.
    pub fn locked(&self) -> bool {
        matches!(self, Key::Fido(k) if k.locked())
    }

    /// Keeps the security key's PIN for the next signature.
    pub fn unlock(&self, pin: &str) -> Result<(), Error> {
        if let Key::Fido(k) = self {
            k.unlock(pin);
        }
        Ok(())
    }

    /// Whether every signature waits on a touch.
    pub fn touch(&self) -> bool {
        matches!(self, Key::Fido(_))
    }

    /// Whether `confirm` needs the PIN first: a security key's.
    pub fn confirm_needs_pin(&self) -> bool {
        self.locked()
    }

    /// Proves the owner is here, signing nothing the server sees: the
    /// Keystore's prompt, or the security key's PIN and a touch. Blocks.
    pub fn confirm(&self, pin: Option<&str>) -> Result<(), Error> {
        match (self, pin) {
            (Key::Keystore(k), _) => k.authenticate(),
            (Key::Fido(k), Some(pin)) => k.confirm(pin),
            (Key::Fido(k), None) => k.sign(&crate::fido::random::<32>()).map(drop),
        }
    }

    /// Removes a Keystore key; a security key's credential stays on it.
    pub fn delete(&self) -> Result<(), Error> {
        match self {
            Key::Keystore(k) => k.delete(),
            Key::Fido(_) => Ok(()),
        }
    }
}

impl From<Keystore> for Key {
    fn from(k: Keystore) -> Key {
        Key::Keystore(k)
    }
}

impl DeviceKey for Key {
    fn key(&self, name: &str) -> mandate_core::Key {
        match self {
            Key::Keystore(k) => k.key(name),
            Key::Fido(k) => k.key(name),
        }
    }

    fn sign(&self, challenge: &[u8; 32]) -> Result<DeviceSignature, Error> {
        match self {
            Key::Keystore(k) => k.sign(challenge),
            Key::Fido(k) => k.sign(challenge),
        }
    }
}

/// The device key in the Android Keystore (Keys.java): P-256, signing only
/// through a BiometricPrompt for each signature.
pub struct Keystore {
    point: [u8; 65],
    alias: String,
}

/// The alias of keys made before each key had its own.
pub const FIRST_ALIAS: &str = "device";

impl Keystore {
    /// A new device key under a fresh alias; any other stays.
    pub fn generate(method: Method) -> Result<Keystore, Error> {
        if method == Method::SecurityKey {
            return Err(Error::Key("A security key is not a Keystore key.".into()));
        }
        let alias = format!("device-{}", &mandate_core::new_token()[..16]);
        let spki = java(|env, c| {
            let name = env.new_string(&alias)?;
            let sig = jni_sig!("(Ljava/lang/String;I)[B");
            keys(env, c, jni_str!("generate"), sig, &[(&name).into(), JValue::Int(method as i32)])
        })?;
        Keystore::from_spki(&spki, alias)
    }

    pub fn load(alias: &str) -> Option<Keystore> {
        let spki = java(|env, c| {
            let name = env.new_string(alias)?;
            let sig = jni_sig!("(Ljava/lang/String;)[B");
            keys(env, c, jni_str!("publicKey"), sig, &[(&name).into()])
        });
        spki.and_then(|spki| Keystore::from_spki(&spki, alias.into()))
            .inspect_err(|e| log::error!("{e}"))
            .ok()
    }

    pub fn alias(&self) -> &str {
        &self.alias
    }

    /// Removes the key from the Keystore; it signs nothing after.
    pub fn delete(&self) -> Result<(), Error> {
        java(|env, c| {
            let name = env.new_string(&self.alias)?;
            let sig = jni_sig!("(Ljava/lang/String;)V");
            env.call_static_method(&c.keys, jni_str!("delete"), sig, &[(&name).into()])?.v()
        })
    }

    pub fn method(&self) -> Method {
        let method = java(|env, c| {
            let name = env.new_string(&self.alias)?;
            let sig = jni_sig!("(Ljava/lang/String;)I");
            env.call_static_method(&c.keys, jni_str!("method"), sig, &[(&name).into()])?.i()
        });
        if method.is_ok_and(|m| m == Method::ScreenLock as i32) {
            Method::ScreenLock
        } else {
            Method::Biometric
        }
    }

    /// Asks the owner to authenticate as this key would, signing nothing.
    /// Blocks; a cancel is an error with Android's message.
    pub fn authenticate(&self) -> Result<(), Error> {
        java(|env, c| {
            let activity = activity(env);
            let alias = env.new_string(&self.alias)?;
            let sig = jni_sig!("(Landroid/app/Activity;Ljava/lang/String;)V");
            let args = [(&activity).into(), (&alias).into()];
            env.call_static_method(&c.keys, jni_str!("authenticate"), sig, &args)?.v()
        })
    }

    /// The point in an X.509 public key, which for P-256 is a fixed header
    /// and then the uncompressed point.
    fn from_spki(spki: &[u8], alias: String) -> Result<Keystore, Error> {
        const HEADER: [u8; 26] = [
            0x30, 0x59, 0x30, 0x13, 0x06, 0x07, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01, 0x06,
            0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07, 0x03, 0x42, 0x00,
        ];
        let point = spki.strip_prefix(&HEADER).and_then(|p| p.try_into().ok());
        let point = point.ok_or_else(|| Error::Key("the Keystore key is not P-256".into()))?;
        Ok(Keystore { point, alias })
    }
}

impl DeviceKey for Keystore {
    fn key(&self, name: &str) -> mandate_core::Key {
        mandate_core::Key::p256(&self.point, name)
    }

    /// Blocks on the owner's answer to the prompt; a cancel signs nothing.
    fn sign(&self, challenge: &[u8; 32]) -> Result<DeviceSignature, Error> {
        let der = java(|env, c| {
            let activity = activity(env);
            let alias = env.new_string(&self.alias)?;
            let msg = env.byte_array_from_slice(challenge)?;
            let sig = jni_sig!("(Landroid/app/Activity;Ljava/lang/String;[B)[B");
            let args = [(&activity).into(), (&alias).into(), (&msg).into()];
            keys(env, c, jni_str!("sign"), sig, &args)
        })?;
        Ok(DeviceSignature { sig: der_signature(&der)?, authenticator_data: Vec::new() })
    }
}

/// Whether a strong biometric is enrolled.
pub fn biometrics_enrolled() -> bool {
    available() & 1 != 0
}

/// Whether a secure screen lock (PIN, pattern or password) is set.
pub fn screen_lock_set() -> bool {
    available() & 2 != 0
}

fn available() -> i32 {
    let bits = java(|env, c| {
        let activity = activity(env);
        let sig = jni_sig!("(Landroid/app/Activity;)I");
        env.call_static_method(&c.keys, jni_str!("available"), sig, &[(&activity).into()])?.i()
    });
    bits.inspect_err(|e| log::error!("available: {e}")).unwrap_or(0)
}

/// The token sealed by a Keystore key.
pub fn seal(token: &str) -> Result<Vec<u8>, Error> {
    java(|env, c| {
        let token = env.byte_array_from_slice(token.as_bytes())?;
        keys(env, c, jni_str!("seal"), jni_sig!("([B)[B"), &[(&token).into()])
    })
}

pub fn unseal(sealed: &[u8]) -> Option<String> {
    let token = java(|env, c| {
        let sealed = env.byte_array_from_slice(sealed)?;
        keys(env, c, jni_str!("unseal"), jni_sig!("([B)[B"), &[(&sealed).into()])
    });
    token.inspect_err(|e| log::error!("{e}")).ok().and_then(|t| String::from_utf8(t).ok())
}

/// Starts the stream service, from the activity.
pub fn listen() {
    let started = java(|env, c| {
        let activity = activity(env);
        let sig = jni_sig!("(Landroid/app/Activity;)V");
        env.call_static_method(&c.stream, jni_str!("start"), sig, &[(&activity).into()])?.v()
    });
    if let Err(e) = started {
        log::error!("starting the stream service: {e}");
    }
}

#[unsafe(no_mangle)]
extern "system" fn Java_dev_mandate_app_Stream_run<'a>(
    mut env: EnvUnowned<'a>,
    this: JObject<'a>,
    dir: JString<'a>,
) {
    let args = env
        .with_env(|env| -> jni::errors::Result<_> {
            Ok(Some((env.new_global_ref(&this)?, dir.try_to_string(env)?)))
        })
        .resolve::<ThrowRuntimeExAndDefault>();
    if let Some((service, dir)) = args {
        serve(Arc::new(service), &PathBuf::from(dir));
    }
}

/// The service, and the items it has a notification up for.
type Notices = (Arc<Global<JObject<'static>>>, Arc<Mutex<HashSet<String>>>);

/// Holds the owner stream for the service, with a notification up for each
/// waiting item. It refetches on each event, and each minute too, since an
/// item can run out of time without one.
fn serve(service: Arc<Global<JObject<'static>>>, dir: &Path) -> ! {
    // Notifications outlive the process, so a new one starts from them.
    let notices: Notices = (service.clone(), Arc::new(Mutex::new(posted(&service))));
    let (tick, tick_dir) = (notices.clone(), dir.to_path_buf());
    std::thread::spawn(move || {
        loop {
            std::thread::sleep(Duration::from_secs(60));
            notify(&tick, Device::load(&tick_dir).as_ref().map(|d| &d.client));
        }
    });
    loop {
        // Loaded on each connect, in case the app has paired anew.
        let device = Device::load(dir);
        match device.as_ref().map(|d| d.client.events()) {
            Some(Ok(events)) => {
                notify(&notices, device.as_ref().map(|d| &d.client));
                for event in events {
                    if event.is_err() {
                        break;
                    }
                    notify(&notices, device.as_ref().map(|d| &d.client));
                }
            }
            Some(Err(e)) => log::warn!("stream: {e}"),
            None => notify(&notices, None),
        }
        std::thread::sleep(Duration::from_secs(5));
    }
}

/// The items with a notification up.
fn posted(service: &Global<JObject<'static>>) -> HashSet<String> {
    let tags = java(|env, _| {
        let sig = jni_sig!("()[Ljava/lang/String;");
        let tags = env.call_method(service, jni_str!("shown"), sig, &[])?.l()?;
        let tags = env.cast_local::<jni::objects::JObjectArray<JString>>(tags)?;
        (0..tags.len(env)?)
            .map(|i| tags.get_element(env, i)?.try_to_string(env))
            .collect::<jni::errors::Result<HashSet<_>>>()
    });
    tags.inspect_err(|e| log::error!("notifications: {e}")).unwrap_or_default()
}

/// Shows what newly waits and hides what no longer does: decided, ended or
/// out of time. Unpaired (no client), nothing waits. A notification names
/// the identity, the connector and the kind of thing, nothing else.
fn notify((service, shown): &Notices, client: Option<&Client>) {
    let waiting = match client.map(crate::waiting) {
        Some(Ok(w)) => w,
        Some(Err(e)) => return log::warn!("stream: {e}"),
        None => Vec::new(),
    };
    let mut shown = shown.lock().unwrap();
    let posted = java(|env, _| {
        for (id, text) in waiting.iter().filter(|(id, _)| !shown.contains(id)) {
            let (id, text) = (env.new_string(id)?, env.new_string(text)?);
            let sig = jni_sig!("(Ljava/lang/String;Ljava/lang/String;)V");
            env.call_method(&**service, jni_str!("show"), sig, &[(&id).into(), (&text).into()])?;
        }
        for id in shown.iter().filter(|id| !waiting.iter().any(|(w, _)| w == *id)) {
            let id = env.new_string(id)?;
            let sig = jni_sig!("(Ljava/lang/String;)V");
            env.call_method(&**service, jni_str!("hide"), sig, &[(&id).into()])?;
        }
        Ok(())
    });
    match posted {
        Ok(()) => *shown = waiting.into_iter().map(|(id, _)| id).collect(),
        Err(e) => log::error!("notifications: {e}"),
    }
}
