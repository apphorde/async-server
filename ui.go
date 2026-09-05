package main

import (
	"fmt"
	"net/http"
)

const page = `<!doctype html>
<html><head><meta name="viewport" content="width=device-width,initial-scale=1"><title>Reader Vault</title>
<style>body{max-width:900px;margin:40px auto;padding:0 16px;font:16px system-ui;background:#f7f8fa;color:#20242b}header{display:flex;justify-content:space-between;align-items:center;border-bottom:3px solid #263b59}h1{letter-spacing:-1px}button,input{font:inherit;padding:8px}button{cursor:pointer;background:#263b59;color:#fff;border:0;border-radius:4px}#login{max-width:330px;margin:15vh auto}.row{display:flex;gap:12px;padding:12px;border-bottom:1px solid #ddd;align-items:center}.row a{color:#173e72;text-decoration:none}.folder{font-weight:600}.muted{color:#667;font-size:.85em;margin-left:auto}.error{color:#a11}.hidden{display:none}</style>
</head><body>
<main id="login"><h1>Reader Vault</h1><p>Sign in to your self-hosted file history.</p>
<input id="email" type="email" placeholder="email" autocomplete="email" required><br><br>
<input id="password" type="password" placeholder="password (12-72 characters)" autocomplete="current-password" minlength="12" maxlength="72" required><br><br>
<button onclick="login()">Sign in</button> <button onclick="register()">Create first account</button><p class="error" id="error"></p></main>
<main id="app" class="hidden"><header><h1>Reader Vault</h1><span id="who"></span><button onclick="logout()">Sign out</button></header><p><button onclick="up()">Up</button> <strong id="crumb">/</strong></p><div id="list"></div><section id="history" class="hidden"></section></main>
<script>
let current=''; const q=s=>document.querySelector(s);
async function api(url,o={}){let r=await fetch(url,o),j=await r.json();if(!r.ok)throw Error(j.error||'request failed');return j}
function credentials(){let p=q('#password');if(!p.reportValidity())throw Error('Password must be 12 to 72 characters.');return {email:q('#email').value,password:p.value}}
async function login(){try{await api('/api/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(credentials())});start()}catch(e){q('#error').textContent=e.message}}
async function register(){try{await api('/api/register',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(credentials())});q('#error').textContent='Account created. Sign in.'}catch(e){q('#error').textContent=e.message}}
async function start(){let m=await api('/api/me');q('#who').textContent=m.email;q('#login').classList.add('hidden');q('#app').classList.remove('hidden');browse()}
function escapeHtml(s){return s.replace(/[&<>'"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;',"'":'&#39;','"':'&quot;'}[c]))}
async function browse(p=current){current=p;q('#crumb').textContent='/'+p;q('#history').classList.add('hidden');let xs=await api('/api/files?path='+encodeURIComponent(p));q('#list').innerHTML=xs.map(x=>'<div class="row '+(x.directory?'folder':'')+'"><a href="#" onclick="openItem('+JSON.stringify(x.path)+','+x.directory+')">'+(x.directory?'[folder] ':'')+escapeHtml(x.path.split('/').pop())+'</a><span class="muted">'+(x.latest?x.latest.size+' bytes, '+new Date(x.latest.created).toLocaleString():'')+'</span></div>').join('')||'<p>No files in this folder.</p>'}
function openItem(p,d){if(d)browse(p);else history(p)} function up(){let i=current.lastIndexOf('/');browse(i<0?'':current.slice(0,i))}
async function history(p){let xs=await api('/api/history?path='+encodeURIComponent(p));q('#history').classList.remove('hidden');q('#history').innerHTML='<h2>'+escapeHtml(p)+'</h2>'+xs.map(v=>'<div class="row"><a href="/api/download?path='+encodeURIComponent(p)+'&version='+v.id+'">Download version</a><span class="muted">'+new Date(v.created).toLocaleString()+' | '+v.size+' bytes | '+v.sha256.slice(0,12)+'</span></div>').join('')}
async function logout(){await api('/api/logout',{method:'POST'});location.reload()} start().catch(()=>{});
</script></body></html>`

func (a *app) ui(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprint(w, page)
}
