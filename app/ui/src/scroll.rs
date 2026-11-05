//! Scrolling as on Android. gpui turns a finger into scroll steps and
//! coasts a fling along Android's own friction curve (gpui-mobile tunes it);
//! what it leaves to the scroll view is the end of the content. Here the
//! content stretches away from an end the finger pulls past, takes up a fling
//! that runs into one, and springs back on the spring of Android's stretch
//! (AOSP's EdgeEffect).
//!
//! A long list stays cheap to scroll: its [`lazy`] rows are laid out only
//! while they show, so a frame costs the screen, not the history.

use gpui::{
    AnyElement, App, AvailableSpace, Bounds, DispatchPhase, Div, ElementId, GlobalElementId,
    Hitbox, HitboxBehavior, InspectorElementId, IntoElement, LayoutId, Pixels, ScrollHandle,
    ScrollWheelEvent, Size, Style, TouchPhase, Window, div, point, prelude::*, px, relative, size,
};
use std::{cell::RefCell, rc::Rc, time::Instant};

/// The spring Android's stretch settles on: natural frequency in radians
/// per second, and damping ratio.
const SPRING: (f32, f32) = (24.657, 0.98);
/// Android hands a fling's speed to the stretch thirteen times over.
const ABSORB: f32 = 13.;
/// How far the content moves at the most stretch, as a share of the view.
const STRETCH: f32 = 0.1;

/// A vertical scroll view: `view` is the window `content` scrolls in. A
/// tracked handle shares the scroll with the screen that reads it.
pub fn view(id: impl Into<ElementId>, view: Div, content: impl IntoElement) -> ScrollView {
    ScrollView {
        id: id.into(),
        view: Some(view),
        content: Some(content.into_any_element()),
        handle: None,
    }
}

pub struct ScrollView {
    id: ElementId,
    view: Option<Div>,
    content: Option<AnyElement>,
    handle: Option<ScrollHandle>,
}

impl ScrollView {
    pub fn track(mut self, handle: &ScrollHandle) -> Self {
        self.handle = Some(handle.clone());
        self
    }
}

impl IntoElement for ScrollView {
    type Element = Self;

    fn into_element(self) -> Self {
        self
    }
}

impl Element for ScrollView {
    type RequestLayoutState = AnyElement;
    type PrepaintState = ();

    fn id(&self) -> Option<ElementId> {
        Some(self.id.clone())
    }

    fn source_location(&self) -> Option<&'static std::panic::Location<'static>> {
        None
    }

    fn request_layout(
        &mut self,
        id: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        window: &mut Window,
        cx: &mut App,
    ) -> (LayoutId, AnyElement) {
        // The scroll and its stretch outlive frames, as long as the view shows.
        let (own, edge) = window.with_element_state(
            id.expect("a scroll view has an id"),
            |state: Option<(ScrollHandle, Rc<RefCell<Edge>>)>, _| {
                let state = state.unwrap_or_default();
                (state.clone(), state)
            },
        );
        let handle = self.handle.take().unwrap_or(own);
        let content = self.content.take().expect("laid out once");
        let mut view = self
            .view
            .take()
            .expect("laid out once")
            .id("view")
            .overflow_y_scroll()
            .track_scroll(&handle)
            .child(Stretch { content, handle, edge })
            .into_any_element();
        (view.request_layout(window, cx), view)
    }

    fn prepaint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        _: Bounds<Pixels>,
        view: &mut AnyElement,
        window: &mut Window,
        cx: &mut App,
    ) {
        view.prepaint(window, cx);
    }

    fn paint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        _: Bounds<Pixels>,
        view: &mut AnyElement,
        _: &mut (),
        window: &mut Window,
        cx: &mut App,
    ) {
        view.paint(window, cx);
    }
}

/// The scrolled content, moved by the stretch. Its scroll listener runs
/// before the view's own (and after any view nested inside), and keeps a
/// finger's or a fling's steps from it.
struct Stretch {
    content: AnyElement,
    handle: ScrollHandle,
    edge: Rc<RefCell<Edge>>,
}

impl IntoElement for Stretch {
    type Element = Self;

    fn into_element(self) -> Self {
        self
    }
}

impl Element for Stretch {
    type RequestLayoutState = ();
    type PrepaintState = (Hitbox, f32);

    fn id(&self) -> Option<ElementId> {
        None
    }

    fn source_location(&self) -> Option<&'static std::panic::Location<'static>> {
        None
    }

    fn request_layout(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        window: &mut Window,
        cx: &mut App,
    ) -> (LayoutId, ()) {
        (self.content.request_layout(window, cx), ())
    }

    fn prepaint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        _: Bounds<Pixels>,
        _: &mut (),
        window: &mut Window,
        cx: &mut App,
    ) -> (Hitbox, f32) {
        // The view's bounds, as its scroll set them this frame.
        let shown = self.handle.bounds();
        let height = f32::from(shown.size.height);
        let hitbox = window.insert_hitbox(shown, HitboxBehavior::Normal);
        let shift = self.edge.borrow_mut().shift(height, Instant::now());
        window.with_element_offset(point(px(0.), px(shift)), |window| {
            self.content.prepaint(window, cx)
        });
        (hitbox, height)
    }

    fn paint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        _: Bounds<Pixels>,
        _: &mut (),
        (hitbox, height): &mut (Hitbox, f32),
        window: &mut Window,
        cx: &mut App,
    ) {
        let (hitbox, height) = (hitbox.clone(), *height);
        let (handle, edge, shell) = (self.handle.clone(), self.edge.clone(), window.current_view());
        window.on_mouse_event(move |e: &ScrollWheelEvent, phase, window, cx| {
            // A wheel's lines, or a touchpad that reports no phases, scroll as
            // gpui scrolls them.
            if phase != DispatchPhase::Bubble
                || !e.delta.precise()
                || !hitbox.should_handle_scroll(window)
            {
                return;
            }
            let dy = f32::from(e.delta.pixel_delta(px(1.)).y);
            let moved =
                edge.borrow_mut().follow(e.touch_phase, dy, &handle, height, Instant::now());
            if let Some(moved) = moved {
                cx.stop_propagation();
                if moved {
                    cx.notify(shell);
                }
            }
        });
        if self.edge.borrow().springing() {
            window.request_animation_frame();
        }
        self.content.paint(window, cx);
    }
}

/// What the finger and a fling do at the ends of one view.
#[derive(Default)]
struct Edge {
    /// A finger is on the view.
    held: bool,
    /// The finger let go; gpui may be coasting a fling, until its last step.
    coasting: bool,
    /// The fling already ran into an end; the rest of it is spent.
    spent: bool,
    /// When the last step came, for the speed a fling hits an end at.
    last: Option<Instant>,
    /// How far the finger has pulled past an end, in px: positive past the
    /// top, negative past the bottom, at most the view's height.
    pull: f32,
    /// The pull settling back to rest, once nothing holds it.
    spring: Option<Spring>,
}

impl Edge {
    /// Takes one scroll step, or leaves it to gpui (`None`): wheel steps,
    /// steps outside a touch, and steps in content that fits, which neither
    /// scrolls nor stretches but may sit in a view that does.
    /// Whether anything moved otherwise.
    fn follow(
        &mut self,
        phase: TouchPhase,
        dy: f32,
        view: &ScrollHandle,
        height: f32,
        now: Instant,
    ) -> Option<bool> {
        match phase {
            // A finger catches what moves: gpui stops the fling, and the
            // stretch stays where it is.
            TouchPhase::Started => {
                if let Some(s) = self.spring.take() {
                    self.pull = s.at(now).0;
                }
                (self.held, self.coasting) = (true, false);
            }
            TouchPhase::Moved | TouchPhase::Ended | TouchPhase::Cancelled
                if self.held || self.coasting => {}
            _ => return None,
        }
        let max = f32::from(view.max_offset().y);
        let mut moved = (max > 0.).then(|| {
            if self.held {
                self.drag(dy, view, max, height)
            } else {
                self.coast(dy, view, max, now)
            }
        });
        if matches!(phase, TouchPhase::Ended | TouchPhase::Cancelled) {
            if self.held {
                // Let go: a stretch springs back; a fling may follow.
                (self.held, self.coasting, self.spent, self.last) = (false, true, false, Some(now));
                if self.pull != 0. {
                    self.spring = Some(Spring { from: (self.pull, 0.), start: now });
                    self.pull = 0.;
                    moved = Some(true);
                }
            } else {
                self.coasting = false;
            }
        }
        moved
    }

    /// The finger moves dy: out of a stretch first, then the content up to
    /// its end, then the stretch past that end.
    fn drag(&mut self, mut dy: f32, view: &ScrollHandle, max: f32, height: f32) -> bool {
        let before = (self.pull, view.offset().y);
        if self.pull * dy < 0. {
            let back = dy.abs().min(self.pull.abs()).copysign(dy);
            self.pull += back;
            dy -= back;
        }
        let at = f32::from(view.offset().y);
        let kept = (at + dy).clamp(-max, 0.);
        self.pull = (self.pull + at + dy - kept).clamp(-height, height);
        view.set_offset(point(px(0.), px(kept)));
        before != (self.pull, view.offset().y)
    }

    /// A fling coasts dy. At an end the stretch takes up its speed and the
    /// rest of it, as Android's stretch absorbs a fling.
    fn coast(&mut self, dy: f32, view: &ScrollHandle, max: f32, now: Instant) -> bool {
        let dt = self.last.map_or(1. / 60., |t| now.duration_since(t).as_secs_f32()).max(0.004);
        self.last = Some(now);
        if self.spent {
            return false;
        }
        let at = f32::from(view.offset().y);
        let kept = (at + dy).clamp(-max, 0.);
        view.set_offset(point(px(0.), px(kept)));
        if kept != at + dy {
            // A stretch still settling at this end goes on from where it is.
            let pull = self.spring.take().map_or(0., |s| s.at(now).0);
            let pull = if pull * dy > 0. { pull } else { 0. };
            self.spring = Some(Spring { from: (pull, dy / dt * ABSORB), start: now });
            self.spent = true;
            return true;
        }
        kept != at
    }

    /// How far the content shows moved, in px, for a view `height` high.
    fn shift(&mut self, height: f32, now: Instant) -> f32 {
        let pull = match self.spring {
            Some(s) => {
                let (pull, speed) = s.at(now);
                let side = if s.from.0 != 0. { s.from.0 } else { s.from.1 };
                if pull * side < 0. || pull.abs() < 0.01 && speed.abs() < 1. {
                    // Through rest, or close enough: done, as Android stops it.
                    self.spring = None;
                    0.
                } else if pull.abs() > height {
                    // No further than a full pull, as Android caps it.
                    let full = height.copysign(pull);
                    self.spring = Some(Spring { from: (full, 0.), start: now });
                    full
                } else {
                    pull
                }
            }
            None => self.pull,
        };
        stretch(pull, height)
    }

    fn springing(&self) -> bool {
        self.spring.is_some()
    }
}

/// How far a pull moves the content: Android's stretch curve, linear plus
/// exponential in the pulled share of the view (dampStretchVector), up to
/// [`STRETCH`] of the view at a full pull.
fn stretch(pull: f32, height: f32) -> f32 {
    if height <= 0. {
        return 0.;
    }
    let d = (pull.abs() / height).min(1.);
    let curve = 0.5 * d + 0.5 * (1. - (-d * std::f32::consts::E / 0.33).exp());
    (STRETCH * height * curve).copysign(pull)
}

/// A pull settling back to rest on Android's stretch spring.
#[derive(Clone, Copy)]
struct Spring {
    /// The pull and its speed (px, px/s) at `start`.
    from: (f32, f32),
    start: Instant,
}

impl Spring {
    /// The pull and its speed at `now`: the underdamped spring in closed form.
    fn at(&self, now: Instant) -> (f32, f32) {
        let (w, z) = SPRING;
        let t = now.duration_since(self.start).as_secs_f32();
        let wd = w * (1. - z * z).sqrt();
        let (x0, v0) = self.from;
        let (a, b) = (x0, (z * w * x0 + v0) / wd);
        let decay = (-z * w * t).exp();
        let (sin, cos) = (wd * t).sin_cos();
        let x = decay * (a * cos + b * sin);
        let v = -z * w * x + decay * wd * (b * cos - a * sin);
        (x, v)
    }
}

/// A row of a long list. Once it has been laid out it keeps its size, and
/// off screen it is only that size: its content is laid out and painted
/// only while it shows. Rows keep their sizes by id, so ids should follow
/// the content rather than its place in the list.
pub fn lazy(id: impl Into<ElementId>, row: impl IntoElement) -> Lazy {
    Lazy { id: id.into(), row: Some(row.into_any_element()) }
}

pub struct Lazy {
    id: ElementId,
    row: Option<AnyElement>,
}

impl IntoElement for Lazy {
    type Element = Self;

    fn into_element(self) -> Self {
        self
    }
}

impl Element for Lazy {
    /// The row, when it was laid out in place.
    type RequestLayoutState = Option<AnyElement>;
    /// The row, when it was laid out on its own because it shows.
    type PrepaintState = Option<AnyElement>;

    fn id(&self) -> Option<ElementId> {
        Some(self.id.clone())
    }

    fn source_location(&self) -> Option<&'static std::panic::Location<'static>> {
        None
    }

    fn request_layout(
        &mut self,
        id: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        window: &mut Window,
        cx: &mut App,
    ) -> (LayoutId, Option<AnyElement>) {
        let known = window.with_element_state(
            id.expect("a lazy row has an id"),
            |known: Option<Option<Size<Pixels>>>, _| {
                let known = known.flatten();
                (known, known)
            },
        );
        match known {
            Some(known) => {
                let style = Style {
                    size: size(relative(1.).into(), known.height.into()),
                    flex_shrink: 0.,
                    ..Style::default()
                };
                (window.request_layout(style, None, cx), None)
            }
            None => {
                let mut row = self.row.take().expect("laid out once");
                (row.request_layout(window, cx), Some(row))
            }
        }
    }

    fn prepaint(
        &mut self,
        id: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        bounds: Bounds<Pixels>,
        placed: &mut Option<AnyElement>,
        window: &mut Window,
        cx: &mut App,
    ) -> Option<AnyElement> {
        let id = id.expect("a lazy row has an id");
        if let Some(row) = placed {
            row.prepaint(window, cx);
            window.with_element_state(id, |_: Option<Option<Size<Pixels>>>, _| {
                ((), Some(bounds.size))
            });
            return None;
        }
        let row = self.row.take().expect("laid out once");
        window.with_element_state(id, |known: Option<Option<Size<Pixels>>>, window| {
            let known = known.flatten();
            if !bounds.intersects(&window.content_mask().bounds) {
                // Off screen it stays a box; a new width wraps it anew, so it
                // is laid out in place next frame.
                return (None, known.filter(|k| k.width == bounds.size.width));
            }
            let mut row = div().w(bounds.size.width).child(row).into_any_element();
            let laid = row.layout_as_root(
                size(AvailableSpace::Definite(bounds.size.width), AvailableSpace::MinContent),
                window,
                cx,
            );
            row.prepaint_at(bounds.origin, window, cx);
            if known != Some(laid) {
                // Its content changed: the list moves to fit it next frame.
                window.refresh();
            }
            (Some(row), Some(laid))
        })
    }

    fn paint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        _: Bounds<Pixels>,
        placed: &mut Option<AnyElement>,
        shown: &mut Option<AnyElement>,
        window: &mut Window,
        cx: &mut App,
    ) {
        if let Some(row) = placed.as_mut().or(shown.as_mut()) {
            row.paint(window, cx);
        }
    }
}
