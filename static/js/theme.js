(function () {
	if (localStorage.getItem('ah4c-theme') === 'light') {
		document.documentElement.setAttribute('data-theme', 'light');
	}
	window.toggleTheme = function () {
		var light = document.documentElement.getAttribute('data-theme') === 'light';
		if (light) {
			document.documentElement.removeAttribute('data-theme');
			localStorage.setItem('ah4c-theme', 'dark');
		} else {
			document.documentElement.setAttribute('data-theme', 'light');
			localStorage.setItem('ah4c-theme', 'light');
		}
	};
	window.toggleNav = function () {
		var nav = document.querySelector('.pagebar .nav, .topbar .nav');
		if (nav) nav.classList.toggle('open');
	};
	window.addEventListener('storage', function (e) {
		if (e.key !== 'ah4c-theme') return;
		if (e.newValue === 'light') {
			document.documentElement.setAttribute('data-theme', 'light');
		} else {
			document.documentElement.removeAttribute('data-theme');
		}
	});
	// /status and /logs shown as panes inside Activity & Logs, which loads them
	// with ?embedded=1. Asked of the address rather than of window.top, which
	// is someone else's whenever ah4c itself sits in a frame, as it does in
	// Organizr: there every page is framed and none of them is a pane.
	var isPane = new URLSearchParams(window.location.search).has('embedded');
	// The build stamp, in the bar on every page.
	//
	// Fetched rather than templated because the twelve pages are static files
	// with a copy of the bar each, and one script they all already load beats
	// twelve edits that drift apart.
	//
	// A narrow bar has no room for it: it collapses to a hamburger and hides
	// its own text with `.pagebar > span { display: none }`, which took the
	// stamp with it, so the version was unreadable on a phone — the one place
	// you cannot check it another way. So the stamp moves to a footer at the
	// end of the page instead. One element, re-parented when the width crosses
	// the same 760px the stylesheet uses, rather than a copy in each place:
	// two copies drift apart the moment either one changes, and a page that
	// prints its own version twice invites the question of which is right.
	function showVersion() {
		var bar = document.querySelector('.pagebar, .topbar');
		if (!bar || document.querySelector('.build-version')) return;
		fetch('/api/version').then(function (r) {
			return r.json();
		}).then(function (d) {
			if (!d || !d.version) return;
			var el = document.createElement('span');
			el.className = 'build-version';
			el.textContent = d.version;
			el.title = 'Build ' + d.version + ' (UTC)';
			// Panes keep theirs in the bar: a footer in each pane of Activity &
			// Logs would stamp the version on the page three times over.
			if (isPane) {
				bar.appendChild(el);
				return;
			}
			var foot = document.createElement('footer');
			foot.className = 'build-footer';
			document.body.appendChild(foot);
			var narrow = window.matchMedia('(max-width: 760px)');
			var place = function () {
				(narrow.matches ? foot : bar).appendChild(el);
			};
			place();
			if (narrow.addEventListener) {
				narrow.addEventListener('change', place);
			} else if (narrow.addListener) {
				narrow.addListener(place);
			}
		}).catch(function () {});
	}
	// Apple TV tuners are driven by pyatv rather than adb, so with PYATV=TRUE
	// ws-scrcpy has no device to show and Device Control is a door to an empty
	// room. Every link to it goes, the menu tile on the home page included —
	// the tile is the same <a href="/device"> the other pages carry in their
	// bars. Fetched for the same reason the build stamp is: thirteen static
	// pages share this script, and one answer beats thirteen edits.
	function hideDeviceControl() {
		fetch('/api/pyatv').then(function (r) {
			return r.json();
		}).then(function (d) {
			if (!d || !d.pyatv) return;
			document.querySelectorAll('a[href="/device"]').forEach(function (a) {
				a.remove();
			});
		}).catch(function () {});
	}
	// The container's hostname stands in for "AH4C" in the tab title and
	// follows the page's name in the bar, "Activity — ah4c2", the way the tab
	// reads, so someone running ah4c, ah4c2 and ah4c3 can tell their pages
	// apart. The home page's name is "AH4C" itself, so there the hostname
	// replaces it rather than following it. The server sends no name when the
	// hostname is Docker's default container ID, and the pages keep "AH4C".
	// Only the exact word is replaced: "AH4C Capture" and the like are names of
	// things in Channels DVR, not of this instance.
	//
	// Panes leave their bar alone: Activity & Logs' own bar already names it.
	function showHostname() {
		fetch('/api/hostname').then(function (r) {
			return r.json();
		}).then(function (d) {
			if (!d || !d.name) return;
			document.title = document.title.replace(/(^|— )AH4C$/, '$1' + d.name);
			if (isPane) return;
			var el = document.querySelector('.pagebar > strong, .topbar > strong');
			if (!el) return;
			el.textContent = el.textContent === 'AH4C' ? d.name : el.textContent + ' — ' + d.name;
		}).catch(function () {});
	}
	function onReady() {
		showVersion();
		hideDeviceControl();
		showHostname();
	}
	if (document.readyState === 'loading') {
		document.addEventListener('DOMContentLoaded', onReady);
	} else {
		onReady();
	}
})();
