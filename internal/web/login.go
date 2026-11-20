package web

// loginHTML 登录页
const loginHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>IPTV-ZT-Bridge · 登录</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;
  background:linear-gradient(135deg,#1a1a2e 0%,#16213e 50%,#0f3460 100%);
  min-height:100vh;display:flex;align-items:center;justify-content:center;
  color:#e0e0e0}
.login-card{background:rgba(255,255,255,0.05);backdrop-filter:blur(20px);
  border:1px solid rgba(255,255,255,0.1);border-radius:16px;padding:40px;
  width:360px;box-shadow:0 20px 60px rgba(0,0,0,0.3)}
h1{font-size:22px;margin-bottom:8px;background:linear-gradient(90deg,#00d4ff,#7b2ff7);
  -webkit-background-clip:text;-webkit-text-fill-color:transparent}
.subtitle{font-size:13px;color:#888;margin-bottom:28px}
.form-group{margin-bottom:18px}
label{display:block;font-size:12px;color:#aaa;margin-bottom:6px;text-transform:uppercase;letter-spacing:1px}
input{width:100%;padding:12px 16px;background:rgba(255,255,255,0.05);
  border:1px solid rgba(255,255,255,0.1);border-radius:8px;color:#fff;
  font-size:14px;transition:all .2s}
input:focus{outline:none;border-color:#00d4ff;background:rgba(0,212,255,0.05)}
button{width:100%;padding:12px;background:linear-gradient(90deg,#00d4ff,#7b2ff7);
  border:none;border-radius:8px;color:#fff;font-size:15px;font-weight:600;
  cursor:pointer;transition:transform .15s,box-shadow .15s}
button:hover{transform:translateY(-1px);box-shadow:0 8px 24px rgba(0,212,255,0.3)}
.error{background:rgba(255,60,60,0.1);border:1px solid rgba(255,60,60,0.3);
  border-radius:8px;padding:10px 14px;margin-bottom:18px;font-size:13px;color:#ff6b6b;display:none}
.logo{width:48px;height:48px;margin-bottom:20px;border-radius:12px;
  background:linear-gradient(135deg,#00d4ff,#7b2ff7);display:flex;align-items:center;justify-content:center;
  font-size:24px}
</style>
</head>
<body>
<div class="login-card">
  <div class="logo">⚡</div>
  <h1>IPTV-ZT-Bridge</h1>
  <p class="subtitle">跨运营商 IPTV 组网桥接</p>
  <div class="error" id="err"></div>
  <form id="f">
    <div class="form-group"><label>用户名</label>
      <input type="text" id="u" value="admin" autocomplete="username"></div>
    <div class="form-group"><label>密码</label>
      <input type="password" id="p" autocomplete="current-password"></div>
    <button type="submit">登录</button>
  </form>
</div>
<script>
document.getElementById('f').addEventListener('submit',async(e)=>{
  e.preventDefault();
  document.getElementById('err').style.display='none';
  try{
    const r=await fetch('/api/login',{method:'POST',
      headers:{'Content-Type':'application/json'},
      body:JSON.stringify({username:document.getElementById('u').value,
        password:document.getElementById('p').value})});
    if(!r.ok) throw new Error((await r.json()).error);
    location.href='/static/index.html';
  }catch(err){
    const el=document.getElementById('err');
    el.textContent=err.message;
    el.style.display='block';
  }
});
</script>
</body>
</html>`
