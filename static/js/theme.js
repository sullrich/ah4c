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
	function showVersion() {
		var bar = document.querySelector('.pagebar, .topbar');
		if (!bar || bar.querySelector('.build-version')) return;
		fetch('/api/version').then(function (r) {
			return r.json();
		}).then(function (d) {
			if (!d || !d.version) return;
			var el = document.createElement('span');
			el.className = 'build-version';
			el.textContent = d.version;
			el.title = 'Build ' + d.version + ' (UTC)';
			bar.appendChild(el);
		}).catch(function () {});
	}
	if (document.readyState === 'loading') {
		document.addEventListener('DOMContentLoaded', showVersion);
	} else {
		showVersion();
	}
})();
