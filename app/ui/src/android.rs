//! Android: the entry point, the Keystore device key, the links and
//! notifications that open the app, and the native half of the stream
//! service. The Java half is in android/java.

use crate::api::{Exchange, Now};
use crate::device::{Device, FILE};
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
use mandate_core::{Client, DeviceKey, Error, der_signature};
use std::collections::HashSet;
use std::ffi::c_void;
use std::panic::AssertUnwindSafe;
use std::path::{Path, PathBuf};
use std::sync::atomic::AtomicU32;
use std::sync::atomic::Ordering::Relaxed;
use std::sync::{LazyLock, Mutex, OnceLock};
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
                    Opened::Insets => cx.notify(),
                });
            }
        })
        .detach();
    });
}

/// What an intent opened the app with, or new insets to lay out for.
enum Opened {
    Link(String),
    Item(String),
    Insets,
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

/// The top and bottom system-bar and cutout insets, in logical px, as bits;
/// atomics so that rendering takes no lock.
static INSETS: [AtomicU32; 2] = [AtomicU32::new(0), AtomicU32::new(0)];

/// The top and bottom insets the window draws under, in logical px.
pub fn insets() -> (f32, f32) {
    let [top, bottom] = &INSETS;
    (f32::from_bits(top.load(Relaxed)), f32::from_bits(bottom.load(Relaxed)))
}

#[unsafe(no_mangle)]
extern "system" fn Java_dev_mandate_app_Main_insets(
    _: EnvUnowned,
    _: JClass,
    top: jni::sys::jfloat,
    bottom: jni::sys::jfloat,
) {
    INSETS[0].store(top.to_bits(), Relaxed);
    INSETS[1].store(bottom.to_bits(), Relaxed);
    let _ = OPENED.0.unbounded_send(Opened::Insets);
}

/// Shows or hides the soft keyboard. NativeActivity hands its keys to gpui
/// as KeyDownEvents, as with a hardware keyboard.
pub fn keyboard(show: bool) {
    if let Some(app) = gpui_jni::android_app() {
        if show { app.show_soft_input(true) } else { app.hide_soft_input(false) }
    }
}

/// The Java classes native code calls. They are found while the library
/// loads, as only then does JNI search the app's own classes.
struct Classes {
    keys: Global<JClass<'static>>,
    stream: Global<JClass<'static>>,
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
        Ok(Classes { keys: env.new_global_ref(keys)?, stream: env.new_global_ref(stream)? })
    });
    match found {
        Ok(classes) => _ = CLASSES.set(classes),
        Err(e) => log::error!("JNI_OnLoad: {e}"),
    }
    jni::sys::JNI_VERSION_1_6
}

/// Runs f with this thread's JNI env; a Java exception becomes an Error
/// with its message.
fn java<T>(f: impl FnOnce(&mut Env, &Classes) -> jni::errors::Result<T>) -> Result<T, Error> {
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
fn activity<'a>(env: &Env<'a>) -> JObject<'a> {
    unsafe { JObject::from_raw(env, gpui_jni::activity_as_ptr() as jni::sys::jobject) }
}

/// The device key in the Android Keystore (Keys.java): P-256, signing only
/// through a BiometricPrompt for each signature.
pub struct Keystore([u8; 65]);

impl Keystore {
    /// A new device key, replacing any before.
    pub fn generate() -> Result<Keystore, Error> {
        let spki = java(|env, c| keys(env, c, jni_str!("generate"), jni_sig!("()[B"), &[]))?;
        Keystore::from_spki(&spki)
    }

    pub fn load() -> Option<Keystore> {
        let spki = java(|env, c| keys(env, c, jni_str!("publicKey"), jni_sig!("()[B"), &[]));
        spki.and_then(|spki| Keystore::from_spki(&spki)).inspect_err(|e| log::error!("{e}")).ok()
    }

    /// The point in an X.509 public key, which for P-256 is a fixed header
    /// and then the uncompressed point.
    fn from_spki(spki: &[u8]) -> Result<Keystore, Error> {
        const HEADER: [u8; 26] = [
            0x30, 0x59, 0x30, 0x13, 0x06, 0x07, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01, 0x06,
            0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07, 0x03, 0x42, 0x00,
        ];
        let point = spki.strip_prefix(&HEADER).and_then(|p| p.try_into().ok());
        point.map(Keystore).ok_or_else(|| Error::Key("the Keystore key is not P-256".into()))
    }
}

impl DeviceKey for Keystore {
    fn public_key(&self) -> [u8; 65] {
        self.0
    }

    /// Blocks on the owner's answer to the prompt; a cancel signs nothing.
    fn sign(&self, msg: &[u8]) -> Result<[u8; 64], Error> {
        let der = java(|env, c| {
            let activity = activity(env);
            let msg = env.byte_array_from_slice(msg)?;
            let sig = jni_sig!("(Landroid/app/Activity;[B)[B");
            keys(env, c, jni_str!("sign"), sig, &[(&activity).into(), (&msg).into()])
        })?;
        der_signature(&der)
    }
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
        serve(&service, &PathBuf::from(dir));
    }
}

/// Holds the owner stream for the service, with a notification up for each
/// waiting item.
fn serve(service: &Global<JObject<'static>>, dir: &Path) -> ! {
    let mut shown = HashSet::new();
    loop {
        // Loaded on each connect, in case the app has paired anew.
        if let Some(device) = Device::load(dir) {
            match device.client.events() {
                Ok(events) => {
                    notify(service, &device.client, &mut shown);
                    for event in events {
                        if event.is_err() {
                            break;
                        }
                        notify(service, &device.client, &mut shown);
                    }
                }
                Err(e) => log::warn!("stream: {e}"),
            }
        }
        std::thread::sleep(Duration::from_secs(5));
    }
}

/// Shows what newly waits and hides what no longer does. A notification
/// names the identity, the connector and the kind of thing, nothing else.
fn notify(service: &Global<JObject<'static>>, client: &Client, shown: &mut HashSet<String>) {
    let waiting = || -> Result<Vec<(String, String)>, Error> {
        let now: Now = client.now(None, None)?;
        let exchanges: Vec<Exchange> = client.get("exchanges")?;
        let exchanges = exchanges.into_iter().map(|e| {
            let what = if e.kind == "device" { "join as a device" } else { "log in" };
            (e.id, format!("{} wants to {what}", e.name))
        });
        let requests = now
            .requests
            .into_iter()
            .map(|r| (r.id, format!("{} wants to read {}", r.identity_name, r.connector)));
        let proposals = now
            .proposals
            .into_iter()
            .map(|p| (p.id, format!("{} wants to {} on {}", p.identity_name, p.op, p.connector)));
        Ok(exchanges.chain(requests).chain(proposals).collect())
    };
    let waiting = match waiting() {
        Ok(w) => w,
        Err(e) => return log::warn!("stream: {e}"),
    };
    let posted = java(|env, _| {
        for (id, text) in waiting.iter().filter(|(id, _)| !shown.contains(id)) {
            let (id, text) = (env.new_string(id)?, env.new_string(text)?);
            let sig = jni_sig!("(Ljava/lang/String;Ljava/lang/String;)V");
            env.call_method(service, jni_str!("show"), sig, &[(&id).into(), (&text).into()])?;
        }
        for id in shown.iter().filter(|id| !waiting.iter().any(|(w, _)| w == *id)) {
            let id = env.new_string(id)?;
            let sig = jni_sig!("(Ljava/lang/String;)V");
            env.call_method(service, jni_str!("hide"), sig, &[(&id).into()])?;
        }
        Ok(())
    });
    match posted {
        Ok(()) => *shown = waiting.into_iter().map(|(id, _)| id).collect(),
        Err(e) => log::error!("notifications: {e}"),
    }
}
