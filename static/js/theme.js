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
			// Framed pages keep theirs in the bar: /status and /logs are shown
			// as panes inside Activity & Logs, and a footer in each pane would
			// stamp the version on the page three times over.
			if (window.self !== window.top) {
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
	if (document.readyState === 'loading') {
		document.addEventListener('DOMContentLoaded', showVersion);
	} else {
		showVersion();
	}
})();
